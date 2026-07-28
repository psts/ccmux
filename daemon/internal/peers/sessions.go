// Session truth: whether a pane holds a Claude SESSION that will actually read
// a message, as opposed to a claude process whose MCP servers happen to be
// loaded. The bus cannot answer this from its own socket — ccmux-peers is a
// child of the process, not of the session, and stays connected while Claude
// sits on the session picker with no conversation at all. Claude Code's
// SessionStart/SessionEnd hooks are the only authoritative signal, so they are
// what presence is built on.
package peers

import (
	"time"

	"ccmux.dev/ccmuxd/internal/model"
)

// sessionIdleGrace is how long a pane with no known running session still
// counts as alive on the strength of recent activity. It covers the case where
// the daemon (or the hook install) missed a SessionStart and only ever sees the
// session's later events.
const sessionIdleGrace = 90 * time.Second

// paneSessions is one pane's session truth: the session ids known to be
// running, and when the pane last showed any sign of life.
type paneSessions struct {
	live         map[string]bool
	lastActivity int64
	// atShell records the backstop observation: the pane's foreground is a bare
	// shell. It is definitive and takes effect at once — the idle grace exists to
	// cover a start we may have MISSED, and there is nothing to miss when we can
	// see the shell. Any positive clears it.
	atShell bool
}

// NoteSession records a verdict about a pane's Claude session. Start and end
// mutate membership by id and SessionNone clears it outright; every other event
// merely refreshes the activity clock, so a sub-agent's events can never leave a
// phantom id behind that keeps a departed pane looking alive forever.
func (s *Service) NoteSession(paneID, sessionID string, sig model.SessionSignal) {
	if paneID == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	ps := s.sessions[paneID]
	if ps == nil {
		ps = &paneSessions{live: map[string]bool{}}
		s.sessions[paneID] = ps
	}
	if sig != model.SessionNone {
		ps.lastActivity = s.Now().UnixMilli()
		ps.atShell = false // a session spoke, so the pane is not idling at a shell
	}
	switch sig {
	case model.SessionStarted:
		ps.live[sessionKey(sessionID)] = true
		ps.atShell = false
	case model.SessionEnded:
		delete(ps.live, sessionKey(sessionID))
	case model.SessionNone:
		// An observation of the PANE, not a report from a session: nothing is
		// running there, so every id we still believe in is stale.
		clear(ps.live)
		ps.atShell = true
	}
	s.persistLocked(paneID, ps)
}

// sessionKey lets a hook with no session_id still participate: all such events
// collapse onto one slot, which is exactly right for a pane that only ever runs
// one interactive session.
func sessionKey(sessionID string) string {
	if sessionID == "" {
		return "-"
	}
	return sessionID
}

// paneSessionDeadLocked reports that a pane is known to hold NO Claude session.
// It answers false unless the bus has positively observed this pane's session
// lifecycle, so a pane whose hooks are missing or misconfigured keeps behaving
// exactly as before: this signal may only ever REMOVE presence, never grant it.
func (s *Service) paneSessionDeadLocked(paneID string) bool {
	ps := s.sessions[paneID]
	if ps == nil || len(ps.live) > 0 {
		return false
	}
	if ps.atShell {
		return true // seen, not inferred — no grace needed
	}
	return s.Now().UnixMilli()-ps.lastActivity >= sessionIdleGrace.Milliseconds()
}

// forgetPaneSessionsLocked drops a pane's session truth once the pane itself is
// gone, so the map tracks live panes rather than growing forever.
func (s *Service) forgetPaneSessionsLocked(paneID string) {
	delete(s.sessions, paneID)
	_ = s.st.DeletePaneSessions(paneID)
}

// persistLocked writes a pane's session truth through, so a daemon restart
// cannot re-admit a pane whose session ended and will never speak again.
func (s *Service) persistLocked(paneID string, ps *paneSessions) {
	ids := make([]string, 0, len(ps.live))
	for id := range ps.live {
		ids = append(ids, id)
	}
	_ = s.st.SavePaneSessions(paneID, ids, ps.lastActivity)
}

// loadSessions rebuilds the in-memory view from the registry at startup.
func (s *Service) loadSessions() {
	stored, err := s.st.LoadPaneSessions()
	if err != nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for paneID, st := range stored {
		ps := &paneSessions{live: make(map[string]bool, len(st.LiveIDs)), lastActivity: st.LastActivity}
		for _, id := range st.LiveIDs {
			ps.live[id] = true
		}
		s.sessions[paneID] = ps
	}
}
