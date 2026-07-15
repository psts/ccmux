package api

import (
	"context"
	"encoding/base64"
	"net/http"

	"github.com/gorilla/websocket"

	"ccmux.dev/ccmuxd/internal/manager"
	"ccmux.dev/ccmuxd/internal/model"
	"ccmux.dev/ccmuxd/internal/session"
)

// wsMsg is the single JSON envelope for all attach traffic (v1: JSON+base64; a
// binary hot-path frame is a later optimization). Bytes travel in Data (base64).
type wsMsg struct {
	T             string          `json:"t"`
	Pane          string          `json:"pane,omitempty"`
	Data          string          `json:"data,omitempty"`
	State         model.Attention `json:"state,omitempty"`
	Cols          int             `json:"cols,omitempty"`
	Rows          int             `json:"rows,omitempty"`
	Panes         []paneInfo      `json:"panes,omitempty"`
	Clients       []ClientInfo    `json:"clients,omitempty"`
	Layout        string          `json:"layout,omitempty"`
	LayoutVersion int             `json:"layoutVersion,omitempty"`
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
	readonly := r.URL.Query().Get("readonly") == "1"
	conn, err := s.upgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	defer conn.Close()

	sub := ctrl.Subscribe()
	defer sub.Close()

	id := s.resolveIdentity(r)
	connID := s.presence.Join(wsID, ClientInfo{
		User:     id.Display,
		Device:   r.URL.Query().Get("device"),
		ReadOnly: readonly,
		Verified: id.Verified,
	}, id.Login, id.Email)
	defer s.presence.Leave(wsID, connID)

	if err := conn.WriteJSON(wsMsg{T: "hello", Panes: paneInfos(ws), Layout: ws.LayoutJSON, LayoutVersion: ws.LayoutVersion}); err != nil {
		return
	}
	sendSnapshots(conn, ctrl, ws)

	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	go s.readLoop(cancel, conn, ctrl, wsID, connID, readonly)
	writeLoop(ctx, conn, ctrl, ws, sub)
}

func orDefault(s, def string) string {
	if s == "" {
		return def
	}
	return s
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

// writeLoop forwards pane output to the client. When the subscriber has lagged
// (a drop occurred), the queued backlog is stale terminal bytes: replaying it
// would layer stale deltas on top of the current screen, so we discard the
// backlog and reseed each pane from a fresh capture instead. The triggering
// event and any buffered control events (attention/presence/layout/pane
// lifecycle) are NOT reseed-recoverable — a snapshot carries only pane bytes —
// so those are delivered after the reseed; only stale "output" is dropped.
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
				pending := sub.Drain()
				if ev.Kind != "output" {
					pending = append([]session.Event{ev}, pending...)
				}
				sendSnapshots(conn, ctrl, ws)
				for _, pe := range pending {
					if err := conn.WriteJSON(frameFor(pe)); err != nil {
						return
					}
				}
				continue
			}
			if err := conn.WriteJSON(frameFor(ev)); err != nil {
				return
			}
		}
	}
}

// readLoop applies client input/resize/focus until the connection closes.
// Read-only observers can focus but cannot send input or resize (server-enforced
// — an observer must never crunch a driver's pane size).
func (s *Server) readLoop(cancel context.CancelFunc, conn *websocket.Conn, ctrl *session.Controller, wsID, connID string, readonly bool) {
	defer cancel()
	for {
		var msg wsMsg
		if err := conn.ReadJSON(&msg); err != nil {
			return
		}
		switch msg.T {
		case "input":
			if readonly {
				continue
			}
			if data, err := base64.StdEncoding.DecodeString(msg.Data); err == nil {
				s.presence.Input(wsID, connID)
				_ = ctrl.SendInput(msg.Pane, data)
			}
		case "resize":
			if readonly {
				continue
			}
			if msg.Cols > 0 && msg.Rows > 0 {
				_ = ctrl.Resize(msg.Pane, msg.Cols, msg.Rows)
			}
		case "focus":
			s.presence.Focus(wsID, connID, msg.Pane)
		}
	}
}

// frameFor renders a session event into its wire frame.
func frameFor(ev session.Event) wsMsg {
	switch ev.Kind {
	case "attention":
		return wsMsg{T: "attention", Pane: ev.PaneID, State: ev.Attention}
	case "presence":
		clients, _ := ev.Payload.([]ClientInfo)
		return wsMsg{T: "presence", Clients: clients}
	case "layout":
		lu, _ := ev.Payload.(manager.LayoutUpdate)
		return wsMsg{T: "layout", Layout: lu.Blob, LayoutVersion: lu.Version}
	case "pane-added", "pane-closed":
		return wsMsg{T: ev.Kind, Pane: ev.PaneID}
	default:
		return wsMsg{T: "output", Pane: ev.PaneID, Data: b64(ev.Data)}
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
