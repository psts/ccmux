package hooks

import (
	"fmt"
	"sync"
	"time"
)

// Background-agent bookkeeping, so a Stop can be told apart from a finish.
//
// Claude Code fires Stop when the main agent stops emitting text. With background
// agents outstanding that is not the end of the work: each agent that finishes
// wakes the main agent, which talks and stops again. One observed run produced
// seven Stops in three minutes for a single piece of work, and every one of them
// announced "finished a task" while up to seven agents were still running.
//
// The counter is deliberately one-sided: an agent is live only if we saw it START.
// SubagentStop fires for agents SubagentStart never announced — 229 stops against
// 87 starts in a day's log, and the ratio swings per session — so a stop for an
// unknown id is ignored rather than counted. That way the count can never go
// negative and never suppresses a real finish because of an agent we never saw.

// agentTTL bounds a leak. An agent that starts and never stops would otherwise
// silence its session's done alerts forever; a real leaked id was present in the
// log this was built from. Ten minutes means the worst case is an alert you would
// have preferred suppressed, not silence.
const agentTTL = 10 * time.Minute

// agentTracker holds the live background agents per Claude session.
type agentTracker struct {
	mu   sync.Mutex
	live map[string]map[string]time.Time // session id -> agent id -> started at
	now  func() time.Time                // swapped in tests
}

func newAgentTracker() *agentTracker {
	return &agentTracker{live: map[string]map[string]time.Time{}, now: time.Now}
}

// observe folds a hook into the tracker. It reports handled=true when the event
// is pure agent bookkeeping and carries no other meaning, so the caller can stop
// there; detail describes what changed, for the trace.
func (t *agentTracker) observe(event, sessionID, agentID string) (handled bool, detail string) {
	switch event {
	case "subagent_start":
		return true, t.start(sessionID, agentID)
	case "subagent_stop":
		return true, t.stop(sessionID, agentID)
	case "session_start":
		// A restarted session cannot still be waiting on the previous one's
		// agents, and their stops will never arrive.
		t.mu.Lock()
		delete(t.live, sessionID)
		t.mu.Unlock()
		return false, "" // still a session signal; the caller must keep going
	}
	return false, ""
}

func (t *agentTracker) start(sessionID, agentID string) string {
	if sessionID == "" || agentID == "" {
		return "unattributable agent (no session or agent id)"
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.live[sessionID] == nil {
		t.live[sessionID] = map[string]time.Time{}
	}
	t.live[sessionID][agentID] = t.now()
	return fmt.Sprintf("%d background agent(s) now live", len(t.live[sessionID]))
}

func (t *agentTracker) stop(sessionID, agentID string) string {
	if sessionID == "" || agentID == "" {
		return "unattributable agent (no session or agent id)"
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	agents := t.live[sessionID]
	if agents == nil {
		return "stop for an agent we never saw start; ignored"
	}
	if _, known := agents[agentID]; !known {
		return "stop for an agent we never saw start; ignored"
	}
	delete(agents, agentID)
	if len(agents) == 0 {
		delete(t.live, sessionID)
	}
	return fmt.Sprintf("%d background agent(s) still live", len(agents))
}

// outstanding counts the session's live agents, dropping any that have outlived
// agentTTL. Expiry happens here rather than on a timer: the only moment the count
// matters is when it is read.
func (t *agentTracker) outstanding(sessionID string) int {
	if sessionID == "" {
		return 0
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	agents := t.live[sessionID]
	cutoff := t.now().Add(-agentTTL)
	for id, started := range agents {
		if started.Before(cutoff) {
			delete(agents, id)
		}
	}
	if len(agents) == 0 {
		delete(t.live, sessionID)
	}
	return len(agents)
}
