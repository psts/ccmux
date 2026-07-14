package api

import (
	"context"
	"encoding/base64"
	"net/http"

	"github.com/gorilla/websocket"

	"ccmux.dev/ccmuxd/internal/model"
	"ccmux.dev/ccmuxd/internal/session"
)

// wsMsg is the single JSON envelope for all attach traffic (v1: JSON+base64; a
// binary hot-path frame is a later optimization). Bytes travel in Data (base64).
type wsMsg struct {
	T     string      `json:"t"`
	Pane  string      `json:"pane,omitempty"`
	Data  string      `json:"data,omitempty"`
	Cols  int         `json:"cols,omitempty"`
	Rows  int         `json:"rows,omitempty"`
	Panes []paneInfo  `json:"panes,omitempty"`
}

type paneInfo struct {
	ID        string          `json:"id"`
	Title     string          `json:"title"`
	CWD       string          `json:"cwd"`
	Attention model.Attention `json:"attention"`
}

// attach upgrades to a WebSocket and streams one workspace's panes. Only this
// (the writer) goroutine writes to the socket; a separate reader goroutine
// consumes client input. Both unwind when the client disconnects.
func (s *Server) attach(w http.ResponseWriter, r *http.Request) {
	wsID := r.URL.Query().Get("workspace")
	ctrl := s.mgr.Controller(wsID)
	ws := s.mgr.Workspace(wsID)
	if ctrl == nil || ws == nil {
		writeError(w, http.StatusConflict, "workspace not live")
		return
	}
	conn, err := s.upgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	defer conn.Close()

	sub := ctrl.Subscribe()
	defer sub.Close()

	if err := conn.WriteJSON(wsMsg{T: "hello", Panes: paneInfos(ws)}); err != nil {
		return
	}
	sendSnapshots(conn, ctrl, ws)

	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	go readLoop(cancel, conn, ctrl)
	writeLoop(ctx, conn, ctrl, ws, sub)
}

// sendSnapshots seeds each pane with its current screen so a fresh attach isn't
// blank.
func sendSnapshots(conn *websocket.Conn, ctrl *session.Controller, ws *model.Workspace) {
	for _, p := range ws.Panes {
		b, err := ctrl.Capture(p.ID, 0)
		if err != nil {
			continue
		}
		_ = conn.WriteJSON(wsMsg{T: "snapshot", Pane: p.ID, Data: b64(b)})
	}
}

// writeLoop forwards pane output to the client, re-seeding a lagged subscriber
// from a fresh capture rather than replaying a corrupted byte stream.
func writeLoop(ctx context.Context, conn *websocket.Conn, ctrl *session.Controller, ws *model.Workspace, sub *session.Sub) {
	for {
		select {
		case <-ctx.Done():
			return
		case ev, ok := <-sub.C:
			if !ok {
				return
			}
			if sub.Lagged() {
				sendSnapshots(conn, ctrl, ws)
			}
			if err := conn.WriteJSON(wsMsg{T: "output", Pane: ev.PaneID, Data: b64(ev.Data)}); err != nil {
				return
			}
		}
	}
}

// readLoop applies client input/resize until the connection closes.
func readLoop(cancel context.CancelFunc, conn *websocket.Conn, ctrl *session.Controller) {
	defer cancel()
	for {
		var msg wsMsg
		if err := conn.ReadJSON(&msg); err != nil {
			return
		}
		switch msg.T {
		case "input":
			if data, err := base64.StdEncoding.DecodeString(msg.Data); err == nil {
				_ = ctrl.SendInput(msg.Pane, data)
			}
		case "resize":
			if msg.Cols > 0 && msg.Rows > 0 {
				_ = ctrl.Resize(msg.Pane, msg.Cols, msg.Rows)
			}
		}
	}
}

func paneInfos(ws *model.Workspace) []paneInfo {
	out := make([]paneInfo, len(ws.Panes))
	for i, p := range ws.Panes {
		out[i] = paneInfo{ID: p.ID, Title: p.Title, CWD: p.CWD, Attention: p.Attention}
	}
	return out
}

func b64(b []byte) string { return base64.StdEncoding.EncodeToString(b) }
