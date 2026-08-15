package api

import (
	"context"
	"encoding/json"
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
// output and accepts no client commands; the one thing a client may send is a
// focus frame reporting presence (see presenceFrames). The opening hello seeds
// current attention so a lens joining mid-session immediately knows what needs
// input (retained-state parity with the per-workspace attach hello).
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
	reader, connID := s.joinFirehose(r)

	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	// Reads start BEFORE the hello write: nothing here blocks between them, and
	// every exit path after the join then has an owner for the presence removal
	// (the deferred conn.Close unwinds the read goroutine).
	s.startFirehoseReads(cancel, conn, reader, connID)

	if err := conn.WriteJSON(firehoseMsg{T: "hello", Attention: currentAttention(s.mgr)}); err != nil {
		return
	}

	// The ticker lives INSIDE this select rather than in a goroutine of its own:
	// this is the connection's only writer, so nothing has to be synchronised,
	// and the soak test's goroutine-count assertion keeps holding.
	ping := time.NewTicker(s.ka.pingEvery())
	defer ping.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ping.C:
			if err := s.ka.writePing(conn); err != nil {
				pingFailed("firehose", reader.login, err)
				return
			}
		case ev, ok := <-ch:
			if !ok {
				return
			}
			if err := conn.WriteJSON(s.firehoseFrame(ev, reader)); err != nil {
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

// firehoseReader is who a firehose connection belongs to, and whether it told us.
//
// The second field is the compatibility hinge. A lens that names itself gets the
// per-reader rule; one that does not gets the pre-existing global rule, so an
// upgraded daemon never goes quiet on a lens that has not been rebuilt yet.
type firehoseReader struct {
	login      string
	identified bool
}

// readerOf is the one place the verified-only rule is encoded. joinFirehose is
// its sole production caller; the alertflag tests pin the rule here directly.
//
// "Identified" deliberately means VERIFIED, not merely self-named. The per-reader
// rule joins this login against a presence entry written by a different socket,
// and those two sockets do not resolve identity the same way:
//
//   - the firehose is dialled at DaemonConfig.wsBaseURL, which on a Mac is
//     loopback, where WhoIs declines and the name falls back to NSFullUserName();
//   - the attach socket for a federated workspace goes DIRECT to the owning host
//     over the tailnet, where WhoIs succeeds and the login is a verified email.
//
// So the join fails for the ordinary Mac-plus-remote-host arrangement, and a
// self-declared name is not evidence enough to act on that failure. An alias
// reconciles the two, but an alias is configuration that nothing here creates.
//
// Requiring verification keeps the sharper per-reader rule where identity is
// actually trustworthy, and everywhere else answers the old global question —
// which is what those lenses got before, so nothing regresses while the alias is
// missing.
func readerOf(id identity) firehoseReader {
	return firehoseReader{login: id.Login, identified: id.Verified}
}

// firehosePresenceWS keys firehose lenses in the presence hub. Not a real
// workspace id (real ones are UUIDs), so its presence broadcasts resolve no
// controller and go nowhere — the entry exists purely for ActiveOwners.
const firehosePresenceWS = "!firehose"

// joinFirehose identifies a firehose connection and registers it in the
// presence hub, so a lens whose ONLY daemon connection is the firehose still
// counts as a person at a screen once it reports presence. Before this, the Mac
// app only reported presence over per-workspace attach sockets — a Mac with no
// hosted workspace attached (or only local panes) was invisible to
// ActiveOwners, so its person's phone buzzed while they sat at the desk.
//
// The entry starts with no presence reported and no focused pane, so it counts
// as at-a-screen only after the lens says so (see client.atAScreen) — a lens
// that never reports, like the hub dialing a member's firehose, adds nothing.
func (s *Server) joinFirehose(r *http.Request) (firehoseReader, string) {
	id := s.resolveIdentity(r)
	connID := s.presence.Join(firehosePresenceWS, ClientInfo{
		User:     id.Display,
		Device:   r.URL.Query().Get("device"),
		ReadOnly: true,
		Verified: id.Verified,
	}, id.Login, id.Email)
	return readerOf(id), connID
}

// presenceFrames returns the one client-frame handler the firehose has: a
// focus frame carrying `present` updates this connection's presence entry.
// Non-focus frames are discarded silently — the firehose carries no commands.
// The two CONTRACT violations are logged instead: the only peer that sends
// frames here is our own app, and a wire drift that silently dropped every
// presence report would reproduce the exact phone-buzzes-at-the-desk bug this
// path exists to fix, with no trace on either end.
func (s *Server) presenceFrames(connID string) func([]byte) {
	return func(data []byte) {
		var msg struct {
			T       string `json:"t"`
			Present *bool  `json:"present"`
		}
		if err := json.Unmarshal(data, &msg); err != nil {
			log.Printf("firehose: dropping an unparseable client frame (%d bytes): %v", len(data), err)
			return
		}
		if msg.T != "focus" {
			return
		}
		if msg.Present == nil {
			log.Printf("firehose: focus frame without a boolean 'present' — presence not updated (wire drift?)")
			return
		}
		s.presence.SetPresent(firehosePresenceWS, connID, *msg.Present)
	}
}

// startFirehoseReads spawns the read goroutine for one firehose connection.
// The READ goroutine owns the presence removal, because it is also the one
// delivering presence frames — removal-after-last-delivery then holds by
// construction. A handler-side defer ran Leave while a buffered focus frame
// could still reach SetPresent, tripping presence.go's should-be-impossible
// warning on every ordinary writer-side exit. WHEN to call this differs per
// handler (see the call sites); the ownership rule lives only here.
func (s *Server) startFirehoseReads(cancel context.CancelFunc, conn *websocket.Conn, reader firehoseReader, connID string) {
	go func() {
		defer s.presence.Leave(firehosePresenceWS, connID)
		drainReads(cancel, conn, s.ka, reader.login, s.presenceFrames(connID))
	}()
}

// firehoseFrame renders a manager firehose Event into its wire frame, stamping
// the alert decision for the lens this frame is being written to.
func (s *Server) firehoseFrame(ev manager.Event, reader firehoseReader) firehoseMsg {
	switch ev.Kind {
	case "attention":
		return firehoseMsg{
			T: "attention", Workspace: ev.WorkspaceID, Pane: ev.PaneID, State: ev.Attention,
			Alert: s.alertsFor(reader, ev.Attention),
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
func (s *Server) alertsFor(reader firehoseReader, att model.Attention) bool {
	if !notifyState(att) {
		return false
	}
	owners := s.focus.ActiveOwners()
	if !reader.identified {
		// Nothing trustworthy to match against, so answer the OLD question — is
		// anybody at a screen — which is exactly what every lens got before this
		// became per-reader. See readerOf for why an unverified name is not
		// enough to act on.
		//
		// This also covers the staged rollout. The daemon and the Mac app ship
		// separately, so there is always a window where an older app talks to an
		// upgraded daemon; without this it would lose every notification, which is
		// the very failure the change exists to fix.
		return len(owners) > 0
	}
	login := reader.login
	if login == "" {
		return false
	}
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

// drainReads hands every client frame to onFrame (which ignores all but the
// presence-reporting focus frame — the firehose carries no commands) and
// cancels the context when the connection closes, unwinding the write loop.
//
// It also owns this connection's read deadline, which is what turns a silently
// dead peer into a closed connection instead of a goroutine parked forever. The
// deadline is armed here, not in the caller, because it must not race the
// ReadMessage below.
func drainReads(cancel context.CancelFunc, conn *websocket.Conn, ka keepalive, who string, onFrame func([]byte)) {
	defer cancel()
	ka.armReads(conn)
	for {
		_, data, err := conn.ReadMessage()
		if err != nil {
			readEnded("firehose", who, err, ka.readWithin())
			return
		}
		ka.touchReads(conn)
		onFrame(data)
	}
}
