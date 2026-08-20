package hooks

import (
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

	r.mu.Lock()
	defer r.mu.Unlock()
	if r.calls != 0 {
		t.Fatalf("attention applied %d times while 2 subagents were running; want 0 (got %q)", r.calls, r.gotAtt)
	}
	// The turn was held, not hidden: stop still proves the session is alive.
	if r.sigCalls != 1 || r.gotSig != model.SessionActive {
		t.Errorf("session signal = %d×%q, want 1×active — a held stop must not look like a dead session", r.sigCalls, r.gotSig)
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

	r.mu.Lock()
	defer r.mu.Unlock()
	if r.calls != 0 {
		t.Fatalf("stops for ids that never started freed the real agent: %d attention calls", r.calls)
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
	if n := l.agents.busy("s1"); n != 1 {
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
		if n := l.agents.busy("s1"); n != 0 {
			t.Errorf("%s left %d agents behind", typ, n)
		}
	}
}

// The other half: an agent whose stop is lost stops counting after agentTTL, so
// the session's NEXT turn-ending event alerts normally. The event already held is
// gone for good — see the comment on agentTTL for why that is deliberate.
func TestAgentTracker_LostStopExpires(t *testing.T) {
	tr := newAgentTracker()
	tr.start("s1", "a1")
	tr.open["s1"]["a1"] = time.Now().Add(-agentTTL - time.Minute)

	if n := tr.busy("s1"); n != 0 {
		t.Fatalf("busy = %d, want an agent older than the TTL to be dropped", n)
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
	if n := tr.busy(""); n != 0 {
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
	if got[0].TraceID != "cafe1234" || got[0].Attention != string(model.AttentionNeedsInput) {
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
	if n := l.agents.busy("s1"); n != 0 {
		t.Errorf("busy = %d, want an unattributable start to track nothing", n)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.calls != 0 {
		t.Errorf("a subagent event applied attention %d times", r.calls)
	}
}
