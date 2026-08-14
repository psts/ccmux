package api

import (
	"context"
	"log"
	"net/http"
	"sort"
	"time"

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
	// Alert tells a lens to raise a notification for this attention, as opposed
	// to only flashing. The DAEMON decides it, because it is the only party that
	// knows both the rule and who is present; a lens that decides for itself is
	// how the Mac app and the push notifier drifted apart twice.
	Alert     bool        `json:"alert,omitempty"`
	Attention []attnEntry `json:"attention,omitempty"` // hello only
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

	// Who is reading this stream. The alert flag is stamped per connection, so a
	// lens is told to notify only when ITS OWN owner is at a screen.
	login := s.resolveIdentity(r).Login

	if err := conn.WriteJSON(firehoseMsg{T: "hello", Attention: currentAttention(s.mgr)}); err != nil {
		return
	}

	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	go drainReads(cancel, conn, s.ka)

	// The ticker lives INSIDE this select rather than in a goroutine of its own:
	// this is the connection's only writer, so nothing has to be synchronised,
	// and the soak test's goroutine-count assertion keeps holding.
	ping := time.NewTicker(s.ka.ping)
	defer ping.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ping.C:
			if err := s.ka.writePing(conn); err != nil {
				pingFailed("firehose", login, err)
				return
			}
		case ev, ok := <-ch:
			if !ok {
				return
			}
			if err := conn.WriteJSON(s.firehoseFrame(ev, login)); err != nil {
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

// firehoseFrame renders a manager firehose Event into its wire frame, stamping
// the alert decision for the lens this frame is being written to.
func (s *Server) firehoseFrame(ev manager.Event, login string) firehoseMsg {
	switch ev.Kind {
	case "attention":
		return firehoseMsg{
			T: "attention", Workspace: ev.WorkspaceID, Pane: ev.PaneID, State: ev.Attention,
			Alert: s.alertsFor(login, ev.Attention),
		}
	default:
		return firehoseMsg{T: ev.Kind, Workspace: ev.WorkspaceID}
	}
}

// alertsFor reports whether THIS lens's owner should raise a notification: the
// state has to be worth alerting on, and that person has to be at a screen.
//
// The rule still lives here rather than in the lens, which is the lesson from the
// Mac app keeping its own copy and going on alerting on `done` long after the
// daemon had stopped pushing on it. What changed is the question. It used to ask
// whether ANYBODY, anywhere in the federation, had a lens at a screen, and
// stamped that one boolean onto the frame every subscriber received. So a
// colleague at their desk could make a sleeping Mac alert, and a person sitting
// right in front of ccmux got nothing whenever the answer happened to be no.
//
// Presence is still federation-wide (s.focus), because a person sits at one
// screen and it does not belong to whichever machine owns the pane that spoke.
// Reading only the local hub made a Linux session's "needs input" arrive at a Mac
// as a silent sidebar flash.
func (s *Server) alertsFor(login string, att model.Attention) bool {
	if !notifyState(att) {
		return false
	}
	if login == "" {
		return false
	}
	owners := s.focus.ActiveOwners()
	if owners[login] {
		s.clearAlertMiss(login)
		return true
	}
	if len(owners) > 0 {
		// Somebody is demonstrably at a screen and it is apparently not this
		// reader. That is usually true and boring. It is also exactly what an
		// identity mismatch looks like, and the two are indistinguishable without
		// naming both sides.
		//
		// The join is genuinely fragile: this login comes from the /v1/events
		// socket (the Mac dials loopback, where WhoIs declines and the name falls
		// back to ?user=), while the presence entry it must match was written by an
		// /v1/attach socket that may have gone direct to a remote host over the
		// tailnet, where WhoIs succeeds and the login is a verified email. An alias
		// is what reconciles them, and an alias is configuration, not an invariant.
		s.noteAlertMiss(login, owners)
	}
	return false
}

// noteAlertMiss says once per login that a firehose reader matched no present
// owner, then stays quiet until that login is seen present again. Latched because
// this sits on the per-attention path and an unlatched line would bury the log.
func (s *Server) noteAlertMiss(login string, owners map[string]bool) {
	s.alertMissMu.Lock()
	defer s.alertMissMu.Unlock()
	if s.alertMissed == nil {
		s.alertMissed = map[string]bool{}
	}
	if s.alertMissed[login] {
		return
	}
	s.alertMissed[login] = true
	log.Printf("firehose: %q matches none of the present lenses %v — no alert will be raised for it (identity alias mismatch?)",
		login, sortedLogins(owners))
}

// clearAlertMiss re-arms the warning for a login now seen present, so a genuine
// mismatch is reported again if it comes back.
func (s *Server) clearAlertMiss(login string) {
	s.alertMissMu.Lock()
	defer s.alertMissMu.Unlock()
	delete(s.alertMissed, login)
}

func sortedLogins(owners map[string]bool) []string {
	out := make([]string, 0, len(owners))
	for login := range owners {
		out = append(out, login)
	}
	sort.Strings(out)
	return out
}

// drainReads discards anything the client sends (the firehose is read-only for
// clients) and cancels the context when the connection closes, unwinding the
// write loop.
//
// It also owns this connection's read deadline, which is what turns a silently
// dead peer into a closed connection instead of a goroutine parked forever. The
// deadline is armed here, not in the caller, because it must not race the
// ReadMessage below.
func drainReads(cancel context.CancelFunc, conn *websocket.Conn, ka keepalive) {
	defer cancel()
	ka.armReads(conn)
	for {
		if _, _, err := conn.ReadMessage(); err != nil {
			return
		}
		ka.touchReads(conn)
	}
}
