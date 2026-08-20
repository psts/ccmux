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

// session is one Claude session's outstanding work.
//
// prompted lives here rather than in a map of its own so that it cannot outlive
// the agents it qualifies: the whole entry is dropped the moment the last agent
// finishes, which is also the moment prompted stops meaning anything.
type session struct {
	agents   map[string]time.Time // agent id → started at
	prompted bool                 // a prompt arrived AFTER these agents started
}

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
	open map[string]*session
	// now is the clock. A test ages an agent past the TTL through this rather
	// than by reaching into the map and binding itself to its shape.
	now func() time.Time
}

func newAgentTracker() *agentTracker {
	return &agentTracker{open: map[string]*session{}, now: time.Now}
}

// start records a subagent as running and returns how many the session now has.
// A session id is required: without one there is nothing to attribute the agent
// to, and a shared empty key would pool every anonymous session together.
func (t *agentTracker) start(sess, agent string) int {
	if sess == "" || agent == "" {
		return 0
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	s := t.open[sess]
	if s == nil {
		s = &session{agents: map[string]time.Time{}}
		t.open[sess] = s
	}
	s.agents[agent] = t.now()
	return len(s.agents)
}

// stop clears a subagent and reports whether it was one we were tracking. The
// false case is the inner-loop stop described on the type: routine, not an error.
func (t *agentTracker) stop(sess, agent string) (known bool, left int) {
	t.mu.Lock()
	defer t.mu.Unlock()
	s := t.open[sess]
	if s == nil {
		return false, 0
	}
	if _, ok := s.agents[agent]; !ok {
		return false, len(s.agents)
	}
	delete(s.agents, agent)
	if len(s.agents) == 0 {
		delete(t.open, sess)
	}
	return true, len(s.agents)
}

// clear forgets a session's agents wholesale, for a session that just started or
// ended. Deliberately NOT called on a new user prompt: a background agent
// commonly reports its stop after the next prompt has already been submitted, so
// clearing there would drop agents that are genuinely still running.
func (t *agentTracker) clear(sess string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	delete(t.open, sess)
}

// notePrompt records whether the human is currently being asked for something,
// so that a later hold can tell a stale flag from a live one.
//
// Only a prompt that arrives while agents are ALREADY running is recorded: one
// from before them was necessarily answered, because Claude could not have
// dispatched an agent while blocked on it. Measured over 322 holds, 31 of the 32
// that met an already-flagged pane were that stale case and exactly 1 was a live
// prompt from a running agent.
func (t *agentTracker) notePrompt(sess string, prompting bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if s := t.open[sess]; s != nil {
		s.prompted = prompting
	}
}

// promptPending reports whether a prompt landed after this session's agents
// started, meaning it may still be blocking one of them.
func (t *agentTracker) promptPending(sess string) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	s := t.open[sess]
	return s != nil && s.prompted
}

// busy reports how many subagents the named session still has running, and how
// many of ITS agents were dropped for outliving agentTTL — a lost stop, which the
// caller traces. Every other session is swept too, so a client that dies without
// a session_end cannot pin its entry there for the daemon's lifetime.
func (t *agentTracker) busy(sess string) (running, expired int) {
	t.mu.Lock()
	defer t.mu.Unlock()
	cutoff := t.now().Add(-agentTTL)
	for id, s := range t.open {
		gone := s.sweep(cutoff)
		if id == sess {
			expired = gone
		}
		if len(s.agents) == 0 {
			delete(t.open, id)
		}
	}
	if s := t.open[sess]; s != nil {
		running = len(s.agents)
	}
	return running, expired
}

// sweep drops every agent that started before cutoff and reports how many went.
func (s *session) sweep(cutoff time.Time) int {
	dropped := 0
	for agent, started := range s.agents {
		if started.Before(cutoff) {
			delete(s.agents, agent)
			dropped++
		}
	}
	return dropped
}

// agentCount renders a count for the trace, where these lines are read next to
// the notification they explain.
func agentCount(n int) string {
	if n == 1 {
		return "1 subagent running"
	}
	return strconv.Itoa(n) + " subagents running"
}
