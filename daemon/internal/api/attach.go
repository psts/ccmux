package api

import (
	"context"
	"encoding/base64"
	"log"
	"net/http"
	"time"

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
	// Present rides along on a client's focus frame: whether this screen is awake
	// and unlocked. A POINTER because absent and false mean different things — a
	// lens too old to report presence must keep the pre-presence behaviour rather
	// than be counted as nobody-is-there. See client.atAScreen.
	Present *bool `json:"present,omitempty"`
}

type paneInfo struct {
	ID        string          `json:"id"`
	Title     string          `json:"title"`
	CWD       string          `json:"cwd"`
	Attention model.Attention `json:"attention"`
	Cols      int             `json:"cols,omitempty"`
	Rows      int             `json:"rows,omitempty"`
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
	// Only the lens that drove the resize is repainted, so the repaint channel is
	// per-connection rather than a broadcast event.
	rs := newResnapper()
	defer rs.stop()
	go s.readLoop(cancel, conn, ctrl, wsID, connID, readonly, rs)
	writeLoop(ctx, conn, ctrl, ws, sub, s.ka, rs)
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
		sendSnapshot(conn, ctrl, p.ID)
	}
}

// snapshotReset homes the cursor and erases the screen. Every snapshot frame is
// prefixed with it so the frame means "this IS the screen" on its own, whatever
// the receiving terminal already held.
//
// capture-pane emits row content with no leading clear, which was safe only while
// snapshots arrived on a blank terminal (a fresh attach). They no longer do: a
// repaint lands on a live screen, and a lens that writes the bytes straight into
// its emulator would staircase them down from wherever the cursor sat. The Mac
// lens clears on its own before painting a snapshot; the web lens does not, and
// it reads the same frame — so the guarantee belongs here, in the frame, not in
// each client. A lens that also clears just clears twice, which costs nothing.
const snapshotReset = "\x1b[H\x1b[2J"

// sendSnapshot seeds (or repaints) one pane. A capture that fails is skipped so
// the lens keeps whatever it had, which beats clearing it to nothing — but it is
// logged, because on the repaint path the screen on display is already wrong and
// "the next resize will retry" means the user has to resize again to fix it.
func sendSnapshot(conn *websocket.Conn, ctrl *session.Controller, paneID string) {
	b, err := ctrl.Capture(paneID, 0)
	if err != nil {
		log.Printf("attach: capture pane %s: %v (lens keeps its current screen)", paneID, err)
		return
	}
	_ = conn.WriteJSON(wsMsg{T: "snapshot", Pane: paneID, Data: b64(append([]byte(snapshotReset), b...))})
}

// writeLoop forwards pane output to the client. When the subscriber has lagged
// (a drop occurred), the queued backlog is stale terminal bytes: replaying it
// would layer stale deltas on top of the current screen, so we discard the
// backlog and reseed each pane from a fresh capture instead. The triggering
// event and any buffered control events (attention/presence/layout/pane
// lifecycle) are NOT reseed-recoverable — a snapshot carries only pane bytes —
// so those are delivered after the reseed; only stale "output" is dropped.
func writeLoop(ctx context.Context, conn *websocket.Conn, ctrl *session.Controller, ws *model.Workspace, sub *session.Sub, ka keepalive, rs *resnapper) {
	// Same placement as the firehose: this loop is the connection's only writer,
	// so folding the ping into its select needs no synchronisation and adds no
	// goroutine. See keepalive.go for why an idle attach needs pinging at all.
	ping := time.NewTicker(ka.pingEvery())
	defer ping.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ping.C:
			if err := ka.writePing(conn); err != nil {
				pingFailed("attach", ws.ID, err)
				return
			}
		case <-rs.wakeups():
			rs.arm()
		case <-rs.due():
			rs.flush(conn, ctrl)
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
func (s *Server) readLoop(cancel context.CancelFunc, conn *websocket.Conn, ctrl *session.Controller, wsID, connID string, readonly bool, rs *resnapper) {
	defer cancel()
	// This goroutine owns the read deadline (see keepalive.go). Without it a lens
	// whose network vanished holds its presence entry open forever, which also
	// keeps suppressing that person's phone pushes.
	s.ka.armReads(conn)
	for {
		var msg wsMsg
		if err := conn.ReadJSON(&msg); err != nil {
			readEnded("attach "+wsID, connID, err, s.ka.readWithin())
			return
		}
		s.ka.touchReads(conn)
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
			// Through the manager so the new size is recorded and broadcast to
			// other lenses (drives the mobile "take over" affordance). It also
			// bounds the size — a lens is not trusted to be sane about it.
			changed, err := s.mgr.ResizePane(msg.Pane, msg.Cols, msg.Rows)
			switch {
			case err != nil:
				// "my pane won't resize" is otherwise a report with no trace behind
				// it: a reaped pane, a resize racing a revive and a dead tmux socket
				// all look identical from the lens.
				log.Printf("attach %s: resize pane %s to %dx%d: %v", wsID, msg.Pane, msg.Cols, msg.Rows, err)
			case changed:
				// The pane just reflowed. A program that repaints on winch has
				// already sent its own deltas; one that does not (a plain shell)
				// would sit here wrapped at the old width, so ask the writer for a
				// fresh capture once the resize stops moving.
				rs.request(msg.Pane)
			}
		case "focus":
			s.presence.Focus(wsID, connID, msg.Pane)
			if msg.Present != nil {
				s.presence.SetPresent(wsID, connID, *msg.Present)
			}
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
	case "pane-size":
		ps, _ := ev.Payload.(manager.PaneSize)
		return wsMsg{T: "pane-size", Pane: ev.PaneID, Cols: ps.Cols, Rows: ps.Rows}
	case "clipboard":
		// Distinct kind is load-bearing: the default arm would write the
		// copied text INTO the terminal as pane output.
		return wsMsg{T: "clipboard", Pane: ev.PaneID, Data: b64(ev.Data)}
	default:
		return wsMsg{T: "output", Pane: ev.PaneID, Data: b64(ev.Data)}
	}
}

func paneInfos(ws *model.Workspace) []paneInfo {
	out := make([]paneInfo, len(ws.Panes))
	for i, p := range ws.Panes {
		out[i] = paneInfo{ID: p.ID, Title: p.Title, CWD: p.CWD, Attention: p.Attention, Cols: p.Cols, Rows: p.Rows}
	}
	return out
}

func b64(b []byte) string { return base64.StdEncoding.EncodeToString(b) }
