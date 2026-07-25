package api

import (
	"context"
	"net/http"

	"github.com/gorilla/websocket"

	"ccmux.dev/ccmuxd/internal/manager"
	"ccmux.dev/ccmuxd/internal/model"
)

// firehoseMsg is the JSON envelope for /v1/events global-firehose frames. It is
// deliberately separate from the attach envelope (wsMsg): the firehose carries
// no pane bytes, only workspace-scoped attention, so every frame names the
// workspace a sidebar lens should flash.
type firehoseMsg struct {
	T         string          `json:"t"`
	Workspace string          `json:"workspace,omitempty"`
	Pane      string          `json:"pane,omitempty"`
	State     model.Attention `json:"state,omitempty"`
	Attention []attnEntry     `json:"attention,omitempty"` // hello only
}

// attnEntry is one pane's current attention in the hello snapshot.
type attnEntry struct {
	Workspace string          `json:"workspace"`
	Pane      string          `json:"pane"`
	State     model.Attention `json:"state"`
}

// events upgrades to a WebSocket and streams global attention changes for every
// live workspace — the sidebar firehose. Unlike /v1/attach it carries no pane
// output and accepts no client commands; a reader goroutine exists only to notice
// the client closing. The opening hello seeds current attention so a lens joining
// mid-session immediately knows what needs input (retained-state parity with the
// per-workspace attach hello).
func (s *Server) events(w http.ResponseWriter, r *http.Request) {
	if s.hub != nil {
		s.hubEvents(w, r) // aggregate every member host's firehose
		return
	}
	conn, err := s.upgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	defer conn.Close()

	// Subscribe before snapshotting so no change is lost in the gap; a change
	// that lands in both is harmless (setting the same attention twice is a no-op
	// for the lens).
	id, ch := s.mgr.SubscribeEvents()
	defer s.mgr.UnsubscribeEvents(id)

	if err := conn.WriteJSON(firehoseMsg{T: "hello", Attention: currentAttention(s.mgr)}); err != nil {
		return
	}

	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	go drainReads(cancel, conn)

	for {
		select {
		case <-ctx.Done():
			return
		case ev, ok := <-ch:
			if !ok {
				return
			}
			if err := conn.WriteJSON(firehoseFrame(ev)); err != nil {
				return
			}
		}
	}
}

// currentAttention snapshots the attention of every pane in every live workspace.
func currentAttention(mgr *manager.Manager) []attnEntry {
	var out []attnEntry
	for _, ws := range mgr.List() {
		if ws.Status != model.StatusLive {
			continue
		}
		for _, p := range ws.Panes {
			out = append(out, attnEntry{Workspace: ws.ID, Pane: p.ID, State: p.Attention})
		}
	}
	return out
}

// firehoseFrame renders a manager firehose Event into its wire frame.
func firehoseFrame(ev manager.Event) firehoseMsg {
	switch ev.Kind {
	case "attention":
		return firehoseMsg{T: "attention", Workspace: ev.WorkspaceID, Pane: ev.PaneID, State: ev.Attention}
	default:
		return firehoseMsg{T: ev.Kind, Workspace: ev.WorkspaceID}
	}
}

// drainReads discards anything the client sends (the firehose is read-only for
// clients) and cancels the context when the connection closes, unwinding the
// write loop.
func drainReads(cancel context.CancelFunc, conn *websocket.Conn) {
	defer cancel()
	for {
		if _, _, err := conn.ReadMessage(); err != nil {
			return
		}
	}
}
