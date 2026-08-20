package hooks

import (
	"strconv"
	"sync"
	"time"
)

// agentTTL bounds how long one unfinished subagent can hold a session's alerts
// back. A subagent that never reports its stop would otherwise mute the pane for
// as long as the daemon runs.
//
// Measured over 548 subagent runs in one trace file: 3 never sent a stop (one of
// them left an agent "running" for 15 hours), and the longest agent that DID
// finish took 18 minutes. 30 minutes clears every real run seen while capping a
// lost stop at half an hour.
//
// What the TTL restores is the session's NEXT turn-ending event, not the one that
// was already held. A held event is dropped, never re-delivered; the comment below
// says why that is deliberate and not an oversight.
const agentTTL = 30 * time.Minute

// A HELD EVENT IS FINAL. This is the one thing the design gives up, written down
// because the obvious "fix" is worse and someone will otherwise implement it.
//
// A held event is dropped. Nothing re-applies it when the last subagent drains,
// so the alert waits for the session's next genuine idle reminder. Replaying 302
// held events from one trace file, 2 never got one: both were sessions whose
// subagent leaked and never reported a stop, so the loss is the leak, not the
// drop.
//
// Re-delivering on drain is the tempting alternative and it costs more than it
// saves. When the main loop does resume after its agents report, its next Stop is
// a median 252 seconds away: only 32% arrive within a minute, and 57% within five.
// A release timer short enough to be a useful alert would therefore fire while the
// model was demonstrably still working, in roughly two thirds of the cases it
// fired at all — reinstating the false "needs your input" this whole file exists
// to remove, just later.

// agentTracker records which subagents each Claude session still has running.
//
// It exists because Claude Code's idle reminder fires 60 seconds after the main
// loop stops talking, whether or not that loop is waiting on background agents.
// Nothing in the notification payload distinguishes the two, so the only way to
// tell "the turn ended" from "the turn is waiting" is to count the agents.
//
// Pairing is by agent id and nothing else. A running subagent also emits a
// SubagentStop at every turn of its own inner loop, each with an id that never
// appeared in a start (1793 of them against 548 real starts in the same trace),
// so a stop for an id we are not tracking is discarded. prompt_id looks like a
// tempting alternative and is not one: it is restamped whenever a new prompt is
// submitted, and background agents routinely outlive the turn that spawned them.
type agentTracker struct {
	mu   sync.Mutex
	open map[string]map[string]time.Time // session id → agent id → started at
}

func newAgentTracker() *agentTracker {
	return &agentTracker{open: map[string]map[string]time.Time{}}
}

// start records a subagent as running and returns how many the session now has.
// A session id is required: without one there is nothing to attribute the agent
// to, and a shared empty key would pool every anonymous session together.
func (t *agentTracker) start(session, agent string) int {
	if session == "" || agent == "" {
		return 0
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.open[session] == nil {
		t.open[session] = map[string]time.Time{}
	}
	t.open[session][agent] = time.Now()
	return len(t.open[session])
}

// stop clears a subagent and reports whether it was one we were tracking. The
// false case is the inner-loop stop described on the type: routine, not an error.
func (t *agentTracker) stop(session, agent string) (known bool, left int) {
	t.mu.Lock()
	defer t.mu.Unlock()
	running := t.open[session]
	if _, ok := running[agent]; !ok {
		return false, len(running)
	}
	delete(running, agent)
	if len(running) == 0 {
		delete(t.open, session)
	}
	return true, len(running)
}

// clear forgets a session's agents wholesale, for a session that just started or
// ended. Deliberately NOT called on a new user prompt: a background agent
// commonly reports its stop after the next prompt has already been submitted, so
// clearing there would drop agents that are genuinely still running.
func (t *agentTracker) clear(session string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	delete(t.open, session)
}

// busy reports how many subagents a session still has running, dropping any that
// have outlived agentTTL first.
func (t *agentTracker) busy(session string) int {
	t.mu.Lock()
	defer t.mu.Unlock()
	running := t.open[session]
	cutoff := time.Now().Add(-agentTTL)
	for agent, started := range running {
		if started.Before(cutoff) {
			delete(running, agent)
		}
	}
	if len(running) == 0 {
		delete(t.open, session)
	}
	return len(running)
}

// agentCount renders a count for the trace, where these lines are read next to
// the notification they explain.
func agentCount(n int) string {
	if n == 1 {
		return "1 subagent running"
	}
	return strconv.Itoa(n) + " subagents running"
}
