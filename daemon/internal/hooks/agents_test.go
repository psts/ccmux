package hooks

import (
	"sync"
	"testing"
	"time"

	"ccmux.dev/ccmuxd/internal/model"
)

func fixedTracker(t *testing.T) (*agentTracker, *time.Time) {
	t.Helper()
	now := time.Date(2026, 7, 30, 12, 0, 0, 0, time.UTC)
	tr := newAgentTracker()
	tr.now = func() time.Time { return now }
	return tr, &now
}

func TestAgentTracker_CountsLiveAgentsPerSession(t *testing.T) {
	tr, _ := fixedTracker(t)

	tr.observe("subagent_start", "s1", "a1")
	tr.observe("subagent_start", "s1", "a2")
	tr.observe("subagent_start", "s2", "a3")

	if got := tr.outstanding("s1"); got != 2 {
		t.Errorf("s1 outstanding = %d, want 2", got)
	}
	if got := tr.outstanding("s2"); got != 1 {
		t.Errorf("s2 outstanding = %d, want 1 (sessions must not pool)", got)
	}

	tr.observe("subagent_stop", "s1", "a1")
	if got := tr.outstanding("s1"); got != 1 {
		t.Errorf("after one stop, s1 = %d, want 1", got)
	}
	tr.observe("subagent_stop", "s1", "a2")
	if got := tr.outstanding("s1"); got != 0 {
		t.Errorf("after both stops, s1 = %d, want 0", got)
	}
}

// SubagentStop fires for agents SubagentStart never announced — 229 stops against
// 87 starts in the log this was built from. Counting those would drive the count
// negative and suppress a genuine finish.
func TestAgentTracker_StopForUnknownAgentIsIgnored(t *testing.T) {
	tr, _ := fixedTracker(t)
	tr.observe("subagent_start", "s1", "a1")

	_, detail := tr.observe("subagent_stop", "s1", "never-started")

	if got := tr.outstanding("s1"); got != 1 {
		t.Errorf("outstanding = %d, want 1; an unknown stop must not decrement", got)
	}
	if detail == "" {
		t.Error("an ignored stop should say so in the trace")
	}
}

// An agent that starts and never stops would otherwise silence its session's done
// alerts forever. A real leaked id was present in the log.
func TestAgentTracker_LeakedAgentExpires(t *testing.T) {
	tr, now := fixedTracker(t)
	tr.observe("subagent_start", "s1", "leaked")

	*now = now.Add(agentTTL - time.Second)
	if got := tr.outstanding("s1"); got != 1 {
		t.Errorf("just inside the TTL: outstanding = %d, want 1", got)
	}

	*now = now.Add(2 * time.Second)
	if got := tr.outstanding("s1"); got != 0 {
		t.Errorf("past the TTL: outstanding = %d, want 0", got)
	}
}

// A restarted session cannot be waiting on the previous one's agents, and their
// stops will never arrive.
func TestAgentTracker_SessionStartClearsAndStaysASignal(t *testing.T) {
	tr, _ := fixedTracker(t)
	tr.observe("subagent_start", "s1", "a1")

	handled, _ := tr.observe("session_start", "s1", "")

	if handled {
		t.Error("session_start must not be consumed here; it is still a session signal the router needs")
	}
	if got := tr.outstanding("s1"); got != 0 {
		t.Errorf("outstanding = %d, want 0 after a session restart", got)
	}
}

func TestAgentTracker_MissingIDsAreNotTracked(t *testing.T) {
	tr, _ := fixedTracker(t)

	tr.observe("subagent_start", "", "a1")
	tr.observe("subagent_start", "s1", "")

	if got := tr.outstanding("s1"); got != 0 {
		t.Errorf("outstanding = %d, want 0; an unattributable agent can't be counted", got)
	}
	if got := tr.outstanding(""); got != 0 {
		t.Errorf("outstanding for an empty session = %d, want 0", got)
	}
}

// Hooks arrive on their own goroutine per connection, so the tracker is written
// concurrently by design.
func TestAgentTracker_ConcurrentObserves(t *testing.T) {
	tr, _ := fixedTracker(t)
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			id := string(rune('a' + i%26))
			tr.observe("subagent_start", "s1", id)
			tr.observe("subagent_stop", "s1", id)
		}(i)
	}
	wg.Wait()
	if got := tr.outstanding("s1"); got != 0 {
		t.Errorf("outstanding = %d, want 0 after every start was matched", got)
	}
}

// The behavior this whole mechanism exists for: seven Stops in three minutes,
// each announcing "finished a task" while up to seven agents were still running.
func TestRoute_StopIsHeldWhileBackgroundAgentsRun(t *testing.T) {
	read := traceTo(t)
	r := &mockRouter{resolve: "pane-1"}
	l := newListener(r)

	l.route(hookMsg{Type: "subagent_start", SessionID: "s1", AgentID: "a1"})
	l.route(hookMsg{Type: "subagent_start", SessionID: "s1", AgentID: "a2"})
	l.route(hookMsg{Type: "stop", SessionID: "s1", CWD: "/repo", PaneID: "pane-1"})

	held := routeOnly(read(), "held")
	if len(held) != 1 {
		t.Fatalf("want 1 held line, got %d", len(held))
	}
	r.mu.Lock()
	calls, att := r.calls, r.gotAtt
	r.mu.Unlock()
	if calls != 0 {
		t.Errorf("router saw %d attention calls (att=%q); a held Stop must set none", calls, att)
	}
}

// The Stop that arrives once the last agent is done is the real finish, and it
// has to notify.
func TestRoute_StopNotifiesOnceTheLastAgentStops(t *testing.T) {
	read := traceTo(t)
	r := &mockRouter{resolve: "pane-1"}
	l := newListener(r)

	l.route(hookMsg{Type: "subagent_start", SessionID: "s1", AgentID: "a1"})
	l.route(hookMsg{Type: "stop", SessionID: "s1", CWD: "/repo", PaneID: "pane-1"})
	l.route(hookMsg{Type: "subagent_stop", SessionID: "s1", AgentID: "a1"})
	l.route(hookMsg{Type: "stop", SessionID: "s1", CWD: "/repo", PaneID: "pane-1"})

	lines := read()
	if got := routeOnly(lines, "held"); len(got) != 1 {
		t.Errorf("want exactly 1 held Stop, got %d", len(got))
	}
	att := routeOnly(lines, "attention")
	if len(att) != 1 || att[0].Attention != string(model.AttentionDone) {
		t.Fatalf("want 1 done attention line, got %+v", att)
	}
}

// A session with no background agents must behave exactly as before — 125 of the
// 161 Stops in the sample log are of this kind.
func TestRoute_StopWithNoAgentsIsUnaffected(t *testing.T) {
	read := traceTo(t)
	l := newListener(&mockRouter{resolve: "pane-1"})

	l.route(hookMsg{Type: "stop", SessionID: "s1", CWD: "/repo", PaneID: "pane-1"})

	if got := routeOnly(read(), "attention"); len(got) != 1 {
		t.Fatalf("want 1 attention line, got %d", len(got))
	}
}

// Only "done" is held. A permission prompt during a background run still needs
// you, and holding it would strand the session waiting for an answer.
func TestRoute_NeedsInputIsNeverHeld(t *testing.T) {
	read := traceTo(t)
	l := newListener(&mockRouter{resolve: "pane-1"})
	l.route(hookMsg{Type: "subagent_start", SessionID: "s1", AgentID: "a1"})

	l.route(hookMsg{Type: "permission_request", SessionID: "s1", CWD: "/repo", PaneID: "pane-1"})

	lines := read()
	if got := routeOnly(lines, "held"); len(got) != 0 {
		t.Errorf("a needs-input event was held: %+v", got)
	}
	att := routeOnly(lines, "attention")
	if len(att) != 1 || att[0].Attention != string(model.AttentionNeedsInput) {
		t.Fatalf("want needs_input applied, got %+v", att)
	}
}

// The subagent events feed the tracker and nothing else: no attention, no pane
// resolution, no session signal.
func TestRoute_SubagentEventsOnlyTrack(t *testing.T) {
	read := traceTo(t)
	r := &mockRouter{resolve: "pane-1"}
	l := newListener(r)

	l.route(hookMsg{Type: "subagent_start", SessionID: "s1", AgentID: "a1", CWD: "/repo", PaneID: "pane-1"})
	l.route(hookMsg{Type: "subagent_stop", SessionID: "s1", AgentID: "a1", CWD: "/repo", PaneID: "pane-1"})

	lines := read()
	if got := routeOnly(lines, "tracked"); len(got) != 2 {
		t.Fatalf("want 2 tracked lines, got %d", len(got))
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.calls != 0 || r.sigCalls != 0 {
		t.Errorf("router saw calls=%d sigCalls=%d; subagent events must touch neither", r.calls, r.sigCalls)
	}
}
