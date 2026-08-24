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
	// Harness/AtShell drive the lens's harness pickers: a shell pane with no
	// harness gets offered the "start here" bar (see model.Pane).
	Harness string `json:"harness,omitempty"`
	AtShell bool   `json:"atShell,omitempty"`
	Dormant bool   `json:"dormant,omitempty"`
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

	// The READ goroutine owns the presence removal, same as the firehose (see
	// startFirehoseReads): readLoop delivers focus/present frames for this
	// connID, and a handler-side deferred Leave ran while a buffered frame
	// could still reach SetPresent — presence.go's should-be-impossible warning
	// on every ordinary writer-side exit. Until the goroutine starts, the two
	// early returns below own the removal explicitly.
	if err := conn.WriteJSON(wsMsg{T: "hello", Panes: paneInfos(ws), Layout: ws.LayoutJSON, LayoutVersion: ws.LayoutVersion}); err != nil {
		s.presence.Leave(wsID, connID)
		return
	}
	if err := sendSnapshots(conn, ctrl, ws); err != nil {
		s.presence.Leave(wsID, connID)
		return
	}

	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	// Only the lens that drove the resize is repainted, so the repaint channel is
	// per-connection rather than a broadcast event.
	rs := newResnapper()
	defer rs.stop()
	go func() {
		defer s.presence.Leave(wsID, connID)
		s.readLoop(cancel, conn, ctrl, wsID, connID, readonly, rs)
	}()
	(&paneWriter{conn: conn, ctrl: ctrl, ws: ws, sub: sub, ka: s.ka, rs: rs}).run(ctx)
}

func orDefault(s, def string) string {
	if s == "" {
		return def
	}
	return s
}

// sendSnapshots seeds each pane with its current screen so a fresh attach isn't
// blank.
func sendSnapshots(conn *websocket.Conn, ctrl *session.Controller, ws *model.Workspace) error {
	for _, p := range ws.Panes {
		if err := sendSnapshot(conn, ctrl, p.ID); err != nil {
			return err
		}
	}
	return nil
}

// snapshotReset homes the cursor and erases the screen. Every snapshot frame is
// prefixed with it so the frame means "this IS the screen" on its own, whatever
// the receiving terminal already held.
//
// capture-pane emits row content with no leading clear, which was safe only while
// snapshots arrived on a blank terminal (a fresh attach). They no longer do: a
// repaint lands on a live screen, and a lens that writes the bytes straight into
// its emulator would paint them starting wherever the cursor happened to be,
// scrolling the live content away instead of replacing it. The Mac
// lens clears on its own before painting a snapshot; the web lens does not, and
// it reads the same frame — so the guarantee belongs here, in the frame, not in
// each client. A lens that also clears just clears twice, which costs nothing.
const snapshotReset = "\x1b[H\x1b[2J"

// sendSnapshot seeds (or repaints) one pane.
//
// The two failures are not the same kind. A capture that fails is per-pane: the
// lens keeps whatever it had, which beats clearing it to nothing, so this returns
// nil and the caller carries on with the other panes. A write that fails is
// per-connection — the socket is gone — so it is returned, and every caller gives
// up the connection on it, like every other write in this file.
//
// Both are logged. A dropped repaint leaves a screen that is already wrong, and
// "the next resize will retry" means the user has to resize again to fix it.
func sendSnapshot(conn *websocket.Conn, ctrl *session.Controller, paneID string) error {
	b, err := ctrl.Capture(paneID, 0)
	if err != nil {
		log.Printf("attach: capture pane %s: %v (lens keeps its current screen)", paneID, err)
		return nil
	}
	if err := conn.WriteJSON(wsMsg{T: "snapshot", Pane: paneID, Data: b64(append([]byte(snapshotReset), b...))}); err != nil {
		log.Printf("attach: send snapshot for pane %s: %v", paneID, err)
		return err
	}
	return nil
}

// reseed recovers a lagged subscriber. The queued backlog is stale terminal
// bytes: replaying it would layer stale deltas on top of the current screen, so
// the backlog is discarded and each pane reseeded from a fresh capture instead.
// The triggering event and any buffered control events (attention/presence/
// layout/pane lifecycle) are NOT reseed-recoverable — a snapshot carries only
// pane bytes — so those are delivered after the reseed; only stale "output" is
// dropped.
func (w *paneWriter) reseed(ev session.Event) error {
	pending := w.sub.Drain()
	if ev.Kind != "output" {
		pending = append([]session.Event{ev}, pending...)
	}
	if err := sendSnapshots(w.conn, w.ctrl, w.ws); err != nil {
		return err
	}
	for _, pe := range pending {
		if err := w.conn.WriteJSON(frameFor(pe)); err != nil {
			return err
		}
	}
	return nil
}

// paneWriter is the write side of one attach connection, and the only goroutine
// that writes to `conn`. Bundling what it needs keeps `step` a dispatcher over
// the four things that can wake it, with each arm's work behind a named method.
type paneWriter struct {
	conn *websocket.Conn
	ctrl *session.Controller
	ws   *model.Workspace
	sub  *session.Sub
	ka   keepalive
	rs   *resnapper
}

// run forwards pane output to the client until the connection ends.
func (w *paneWriter) run(ctx context.Context) {
	// Same placement as the firehose: this loop is the connection's only writer,
	// so folding the ping into its select needs no synchronisation and adds no
	// goroutine. See keepalive.go for why an idle attach needs pinging at all.
	ping := time.NewTicker(w.ka.pingEvery())
	defer ping.Stop()

	for {
		if done := w.step(ctx, ping.C); done {
			return
		}
	}
}

// step handles exactly one wake-up. Every write error ends the connection: the
// socket is the one resource all four arms share, so a failure on any of them
// says the same thing.
func (w *paneWriter) step(ctx context.Context, ping <-chan time.Time) (done bool) {
	select {
	case <-ctx.Done():
		return true
	case <-ping:
		if err := w.ka.writePing(w.conn); err != nil {
			pingFailed("attach", w.ws.ID, err)
			return true
		}
	case <-w.rs.wakeups():
		w.rs.arm()
	case <-w.rs.due():
		if err := w.rs.flush(w.conn, w.ctrl); err != nil {
			return true
		}
	case ev, ok := <-w.sub.C:
		if !ok {
			return true
		}
		if err := w.forward(ev); err != nil {
			return true
		}
	}
	return false
}

// forward sends one event, or recovers the whole screen when the subscriber has
// fallen behind.
func (w *paneWriter) forward(ev session.Event) error {
	if w.sub.Lagged() {
		return w.reseed(ev)
	}
	return w.conn.WriteJSON(frameFor(ev))
}

// applyInput decodes a keystroke frame and sends it to the pane.
//
// Both failures used to vanish — an undecodable frame fell out of an `err == nil`
// with no else, and the send error went into `_`. That is the same blindness the
// resize arm below was given logging for, and worse here: input loss is
// per-keystroke, so a user can lose half a command and not see which half.
//
// Presence is recorded only after the send succeeds; marking "this user typed"
// for input that never reached tmux is a claim about a screen nobody made.
func (s *Server) applyInput(ctrl *session.Controller, msg wsMsg, wsID, connID string) {
	data, err := base64.StdEncoding.DecodeString(msg.Data)
	if err != nil {
		log.Printf("attach %s: undecodable input frame for pane %s (%d chars): %v",
			wsID, msg.Pane, len(msg.Data), err)
		return
	}
	if err := ctrl.SendInput(msg.Pane, data); err != nil {
		log.Printf("attach %s: send input to pane %s (%d bytes): %v", wsID, msg.Pane, len(data), err)
		return
	}
	s.presence.Input(wsID, connID)
}

// applyResize drives the pane to the size this lens is showing.
//
// Through the manager so the new size is recorded and broadcast to the other
// lenses (which drives the mobile "take over" affordance), and so it is bounded
// — a lens is not trusted to be sane about it.
func (s *Server) applyResize(msg wsMsg, wsID string, rs *resnapper) {
	changed, err := s.mgr.ResizePane(msg.Pane, msg.Cols, msg.Rows)
	switch {
	case err != nil:
		// "my pane won't resize" is otherwise a report with no trace behind it: a
		// reaped pane, a resize racing a revive and a dead tmux socket all look
		// identical from the lens.
		log.Printf("attach %s: resize pane %s to %dx%d: %v", wsID, msg.Pane, msg.Cols, msg.Rows, err)
	case changed:
		// The pane just reflowed. A program that repaints on winch has already
		// sent its own deltas; one that does not (a plain shell) would sit here
		// wrapped at the old width, so ask the writer for a fresh capture once the
		// resize stops moving.
		rs.request(msg.Pane)
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
			s.applyInput(ctrl, msg, wsID, connID)
		case "resize":
			if readonly {
				continue
			}
			s.applyResize(msg, wsID, rs)
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
		out[i] = paneInfo{
			ID: p.ID, Title: p.Title, CWD: p.CWD, Attention: p.Attention,
			Cols: p.Cols, Rows: p.Rows,
			Harness: p.Harness, AtShell: p.AtShell, Dormant: p.Dormant,
		}
	}
	return out
}

func b64(b []byte) string { return base64.StdEncoding.EncodeToString(b) }
