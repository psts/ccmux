package hooks

import (
	"strings"
	"testing"
	"time"

	"ccmux.dev/ccmuxd/internal/model"
)

// The reason this whole file exists: two Explore agents started, the main loop
// went quiet, and 60 seconds later Claude Code's idle reminder claimed the
// workspace was waiting for a human. It was waiting for the agents.
func TestRoute_IdleReminderIsHeldWhileAgentsRun(t *testing.T) {
	r := &mockRouter{resolve: "pane-1"}
	l := newListener(r)

	l.route(hookMsg{Type: "subagent_start", SessionID: "s1", PaneID: "pane-1", AgentID: "a1"})
	l.route(hookMsg{Type: "subagent_start", SessionID: "s1", PaneID: "pane-1", AgentID: "a2"})
	l.route(hookMsg{Type: "stop", SessionID: "s1", PaneID: "pane-1", CWD: "/repo"})
	l.route(hookMsg{Type: "notification", NotificationType: "idle_prompt", SessionID: "s1", PaneID: "pane-1", CWD: "/repo"})

	if r.applied(model.AttentionNeedsInput) || r.applied(model.AttentionDone) {
		t.Fatalf("pane was told the turn ended while 2 subagents were running: %v", r.atts)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	// The turn was held, not hidden: stop still proves the session is alive.
	if r.sigCalls != 1 || r.gotSig != model.SessionActive {
		t.Errorf("session signal = %d×%q, want 1×active — a held stop must not look like a dead session", r.sigCalls, r.gotSig)
	}
}

// Attention is sticky, so dropping a held event is not enough: a pane already
// flagged by an answered permission prompt would keep claiming it needs you for
// the whole agent run. 32 of 322 holds in one trace met an already-flagged pane.
func TestRoute_HoldClearsAStaleFlag(t *testing.T) {
	r := &mockRouter{resolve: "pane-1"}
	l := newListener(r)

	// A prompt, answered by the user, and only then the agents.
	l.route(hookMsg{Type: "notification", NotificationType: "permission_prompt", SessionID: "s1", PaneID: "pane-1", CWD: "/repo"})
	l.route(hookMsg{Type: "subagent_start", SessionID: "s1", PaneID: "pane-1", AgentID: "a1"})
	l.route(hookMsg{Type: "stop", SessionID: "s1", PaneID: "pane-1", CWD: "/repo"})

	r.mu.Lock()
	defer r.mu.Unlock()
	if r.gotAtt != model.AttentionIdle {
		t.Fatalf("pane left showing %q; a turn that is still working is not waiting on you", r.gotAtt)
	}
}

// The exception, and the reason the hold cannot simply always clear: a prompt
// that arrived AFTER the agents started may be a running agent blocked right
// now, and clearing its flag would strand it in silence.
func TestRoute_HoldLeavesALivePromptAlone(t *testing.T) {
	r := &mockRouter{resolve: "pane-1"}
	l := newListener(r)

	l.route(hookMsg{Type: "subagent_start", SessionID: "s1", PaneID: "pane-1", AgentID: "a1"})
	l.route(hookMsg{Type: "permission_request", SessionID: "s1", PaneID: "pane-1", CWD: "/repo"})
	before := len(r.atts)
	l.route(hookMsg{Type: "stop", SessionID: "s1", PaneID: "pane-1", CWD: "/repo"})

	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.atts) != before {
		t.Fatalf("the stop overwrote a live prompt: %v", r.atts)
	}
	if r.gotAtt != model.AttentionNeedsInput {
		t.Fatalf("pane shows %q, want the agent's own prompt still standing", r.gotAtt)
	}
}

// A hold is scoped to the session that owns the agents. Without this, one busy
// session would mute every pane on the machine and every other test would pass.
func TestRoute_HoldIsScopedToItsSession(t *testing.T) {
	r := &mockRouter{resolve: "pane-2"}
	l := newListener(r)

	l.route(hookMsg{Type: "subagent_start", SessionID: "s1", PaneID: "pane-1", AgentID: "a1"})
	l.route(hookMsg{Type: "notification", NotificationType: "idle_prompt", SessionID: "s2", PaneID: "pane-2", CWD: "/other"})

	r.mu.Lock()
	defer r.mu.Unlock()
	if r.calls != 1 || r.gotAtt != model.AttentionNeedsInput || r.gotPane != "pane-2" {
		t.Fatalf("got %d×%q on %q; s2 has no agents and must alert normally", r.calls, r.gotAtt, r.gotPane)
	}
}

// A turn-ending hook with no session id cannot be checked against anything. The
// alert goes through, and the trace has to say the hold was never consulted.
func TestRouteTrace_HoldWithoutASessionIsRecorded(t *testing.T) {
	read := traceTo(t)
	r := &mockRouter{resolve: "pane-1"}
	l := newListener(r)

	l.route(hookMsg{Type: "notification", NotificationType: "idle_prompt", PaneID: "pane-1", CWD: "/repo"})

	if got := routeOnly(read(), "hold-unchecked"); len(got) != 1 {
		t.Fatalf("want 1 hold-unchecked line, got %d", len(got))
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.gotAtt != model.AttentionNeedsInput {
		t.Errorf("attention = %q; an uncheckable hook must not be silently held", r.gotAtt)
	}
}

// Evicting a leaked agent is a state change like any other, and the only one
// that used to happen without a line. A run of held alerts that simply stops is
// exactly the shape a debugger cannot explain.
func TestRouteTrace_ExpiredAgentIsRecorded(t *testing.T) {
	read := traceTo(t)
	r := &mockRouter{resolve: "pane-1"}
	l := newListener(r)
	now := time.Now()
	l.agents.now = func() time.Time { return now }

	l.route(hookMsg{Type: "subagent_start", SessionID: "s1", PaneID: "pane-1", AgentID: "a1"})
	now = now.Add(agentTTL + time.Minute)
	l.route(hookMsg{Type: "notification", NotificationType: "idle_prompt", SessionID: "s1", PaneID: "pane-1", CWD: "/repo"})

	got := routeOnly(read(), "agent-expired")
	if len(got) != 1 {
		t.Fatalf("want 1 agent-expired line, got %d", len(got))
	}
	if !strings.Contains(got[0].Detail, "never reported a stop") {
		t.Errorf("detail = %q, want it to name the lost stop", got[0].Detail)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.gotAtt != model.AttentionNeedsInput {
		t.Errorf("attention = %q; past the TTL the alert must land", r.gotAtt)
	}
}

// A session that dies without a session_end must not pin its entry in the
// tracker for the daemon's lifetime.
func TestAgentTracker_SweepReachesEverySession(t *testing.T) {
	tr := newAgentTracker()
	now := time.Now()
	tr.now = func() time.Time { return now }
	tr.start("dead", "a1")
	tr.start("live", "a2")

	now = now.Add(agentTTL + time.Minute)
	tr.start("live", "a3") // fresh, so "live" survives the sweep
	if running, _ := tr.busy("live"); running != 1 {
		t.Fatalf("live session has %d agents, want only the fresh one", running)
	}
	if _, ok := tr.open["dead"]; ok {
		t.Error("a session nobody asked about kept its entry through the sweep")
	}
}

// Once the agents report, the next idle reminder is the truth and must land.
func TestRoute_IdleReminderLandsOnceAgentsFinish(t *testing.T) {
	r := &mockRouter{resolve: "pane-1"}
	l := newListener(r)

	l.route(hookMsg{Type: "subagent_start", SessionID: "s1", PaneID: "pane-1", AgentID: "a1"})
	l.route(hookMsg{Type: "subagent_stop", SessionID: "s1", PaneID: "pane-1", AgentID: "a1"})
	l.route(hookMsg{Type: "notification", NotificationType: "idle_prompt", SessionID: "s1", PaneID: "pane-1", CWD: "/repo"})

	r.mu.Lock()
	defer r.mu.Unlock()
	if r.calls != 1 || r.gotAtt != model.AttentionNeedsInput {
		t.Fatalf("got %d×%q, want 1×needs_input once no agent is running", r.calls, r.gotAtt)
	}
}

// A subagent emits a SubagentStop at every turn of its own inner loop, under an
// id that never started — 1793 of them against 548 real starts in one trace. If
// those decremented anything, one long agent would unmute itself immediately.
func TestRoute_InnerLoopStopDoesNotFreeTheAgent(t *testing.T) {
	r := &mockRouter{resolve: "pane-1"}
	l := newListener(r)

	l.route(hookMsg{Type: "subagent_start", SessionID: "s1", PaneID: "pane-1", AgentID: "real"})
	for _, id := range []string{"turn-1", "turn-2", "turn-3"} {
		l.route(hookMsg{Type: "subagent_stop", SessionID: "s1", PaneID: "pane-1", AgentID: id})
	}
	l.route(hookMsg{Type: "notification", NotificationType: "idle_prompt", SessionID: "s1", PaneID: "pane-1", CWD: "/repo"})

	if r.applied(model.AttentionNeedsInput) {
		t.Fatalf("stops for ids that never started freed the real agent: %v", r.atts)
	}
}

// Being blocked on the human is not being finished. Holding a permission prompt
// would strand the agents that are being waited on.
func TestRoute_BlockingPromptsAreNeverHeld(t *testing.T) {
	for _, c := range []struct{ typ, notif string }{
		{"notification", "permission_prompt"},
		{"notification", "elicitation_dialog"},
		{"permission_request", ""},
		{"ask_user_question", ""},
	} {
		r := &mockRouter{resolve: "pane-1"}
		l := newListener(r)
		l.route(hookMsg{Type: "subagent_start", SessionID: "s1", PaneID: "pane-1", AgentID: "a1"})
		l.route(hookMsg{Type: c.typ, NotificationType: c.notif, SessionID: "s1", PaneID: "pane-1", CWD: "/repo"})

		r.mu.Lock()
		ok := r.calls == 1 && r.gotAtt == model.AttentionNeedsInput
		got, att := r.calls, r.gotAtt
		r.mu.Unlock()
		if !ok {
			t.Errorf("%s/%s: got %d×%q while an agent ran, want 1×needs_input", c.typ, c.notif, got, att)
		}
	}
}

// A background agent commonly reports its stop after the next prompt has been
// submitted, so a new prompt must not be taken as proof its agents are gone.
func TestRoute_NewPromptDoesNotForgetRunningAgents(t *testing.T) {
	r := &mockRouter{resolve: "pane-1"}
	l := newListener(r)

	l.route(hookMsg{Type: "subagent_start", SessionID: "s1", PaneID: "pane-1", AgentID: "a1"})
	l.route(hookMsg{Type: "user_prompt_submit", SessionID: "s1", PaneID: "pane-1", CWD: "/repo"})
	if n, _ := l.agents.busy("s1"); n != 1 {
		t.Fatalf("busy = %d after a new prompt, want the running agent still counted", n)
	}
}

// A session that restarts keeps no agents from its predecessor, which is the
// cheap half of not muting a pane forever.
func TestRoute_SessionLifecycleClearsAgents(t *testing.T) {
	for _, typ := range []string{"session_start", "session_end"} {
		l := newListener(&mockRouter{resolve: "pane-1"})
		l.route(hookMsg{Type: "subagent_start", SessionID: "s1", PaneID: "pane-1", AgentID: "a1"})
		l.route(hookMsg{Type: typ, SessionID: "s1", PaneID: "pane-1", CWD: "/repo"})
		if n, _ := l.agents.busy("s1"); n != 0 {
			t.Errorf("%s left %d agents behind", typ, n)
		}
	}
}

// The other half: an agent whose stop is lost stops counting after agentTTL, so
// the session's NEXT turn-ending event alerts normally. The event already held is
// gone for good — see the comment on agentTTL for why that is deliberate.
func TestAgentTracker_LostStopExpires(t *testing.T) {
	tr := newAgentTracker()
	now := time.Now()
	tr.now = func() time.Time { return now }
	tr.start("s1", "a1")
	now = now.Add(agentTTL + time.Minute)

	running, expired := tr.busy("s1")
	if running != 0 || expired != 1 {
		t.Fatalf("busy = (%d running, %d expired), want (0, 1) past the TTL", running, expired)
	}
	if _, ok := tr.open["s1"]; ok {
		t.Error("an emptied session should be forgotten, not left as a husk")
	}
}

// Agents belong to a session. Without a session id there is nothing to attribute
// them to, and pooling them under a shared empty key would let one anonymous
// session mute another.
func TestAgentTracker_IgnoresAgentsWithoutASession(t *testing.T) {
	tr := newAgentTracker()
	if n := tr.start("", "a1"); n != 0 {
		t.Errorf("start with no session counted %d", n)
	}
	if n := tr.start("s1", ""); n != 0 {
		t.Errorf("start with no agent id counted %d", n)
	}
	if n, _ := tr.busy(""); n != 0 {
		t.Errorf("busy = %d for a session id that does not exist", n)
	}
}

func TestEndsTurn(t *testing.T) {
	cases := []struct {
		typ, notif string
		want       bool
	}{
		{"stop", "", true},
		{"notification", "idle_prompt", true},
		{"notification", "permission_prompt", false},
		{"notification", "elicitation_dialog", false},
		{"permission_request", "", false},
		{"ask_user_question", "", false},
		{"user_prompt_submit", "", false},
		{"session_end", "", false},
	}
	for _, c := range cases {
		if got := endsTurn(c.typ, c.notif); got != c.want {
			t.Errorf("endsTurn(%q,%q) = %v, want %v", c.typ, c.notif, got, c.want)
		}
	}
}

// The trace is where a held alert is explained; without these lines a reader
// sees a notification that never happened and no reason for it.
func TestRouteTrace_HeldAlertSaysWhy(t *testing.T) {
	read := traceTo(t)
	l := newListener(&mockRouter{resolve: "pane-1"})

	l.route(hookMsg{Type: "subagent_start", SessionID: "s1", PaneID: "pane-1", AgentID: "a1"})
	l.route(hookMsg{Type: "notification", NotificationType: "idle_prompt", SessionID: "s1", PaneID: "pane-1", CWD: "/repo", TraceID: "cafe1234"})

	if got := routeOnly(read(), "agent-start"); len(got) != 1 || got[0].Detail != "1 subagent running" {
		t.Fatalf("agent-start lines = %+v", got)
	}
	got := routeOnly(read(), "held")
	if len(got) != 1 {
		t.Fatalf("want 1 held line, got %d", len(got))
	}
	// The line records what the pane was given INSTEAD, which is the thing a
	// reader chasing a missing alert needs to see.
	if got[0].TraceID != "cafe1234" || got[0].Attention != string(model.AttentionIdle) {
		t.Errorf("held line cannot be tied back to its hook: %+v", got[0])
	}
	if got[0].Detail != "1 subagent running" {
		t.Errorf("detail = %q, want the count that explains the hold", got[0].Detail)
	}
}

// An unattributable subagent event is the one failure that would be invisible:
// the hold silently stops working and no other line would ever say so.
func TestRoute_UnattributableAgentIsLoud(t *testing.T) {
	read := traceTo(t)
	r := &mockRouter{resolve: "pane-1"}
	l := newListener(r)

	l.route(hookMsg{Type: "subagent_start", SessionID: "s1", PaneID: "pane-1"}) // no agent id
	l.route(hookMsg{Type: "subagent_start", PaneID: "pane-1", AgentID: "a1"})   // no session id

	if got := routeOnly(read(), "agent-unattributed"); len(got) != 2 {
		t.Fatalf("want 2 agent-unattributed lines, got %d", len(got))
	}
	// It must not half-work: nothing tracked, and no attention touched either.
	if n, _ := l.agents.busy("s1"); n != 0 {
		t.Errorf("busy = %d, want an unattributable start to track nothing", n)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.calls != 0 {
		t.Errorf("a subagent event applied attention %d times", r.calls)
	}
}
