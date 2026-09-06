package manager

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"ccmux.dev/ccmuxd/internal/model"
	"ccmux.dev/ccmuxd/internal/store"
	"ccmux.dev/ccmuxd/internal/tmux"
)

func TestDecideAgent(t *testing.T) {
	now := time.Date(2026, 9, 5, 12, 0, 0, 0, time.UTC)
	tenMin := 10 * time.Minute
	cases := []struct {
		name string
		v    agentView
		want agentAction
	}{
		{"asleep, on demand: stays asleep", agentView{atShell: true}, actNone},
		{"asleep, keep-alive: wake", agentView{atShell: true, keepAlive: true}, actWake},
		{"busy: nothing", agentView{idle: false, idleExit: tenMin}, actNone},
		{"idle but recent: nothing", agentView{idle: true, idleSince: now.Add(-time.Minute), idleExit: tenMin}, actNone},
		{"idle past cap: exit", agentView{idle: true, idleSince: now.Add(-11 * time.Minute), idleExit: tenMin}, actExit},
		{"idle past cap with open task: nothing", agentView{idle: true, idleSince: now.Add(-time.Hour), idleExit: tenMin, openTasks: 1}, actNone},
		{"idle, no cap configured: nothing", agentView{idle: true, idleSince: now.Add(-time.Hour), idleExit: 0}, actNone},
		{"keep-alive idle, same base: stays up", agentView{idle: true, idleSince: now.Add(-time.Hour), idleExit: tenMin, keepAlive: true}, actNone},
		{"keep-alive idle, base moved: exit so the wake picks it up", agentView{idle: true, idleSince: now.Add(-time.Hour), idleExit: tenMin, keepAlive: true, drift: true}, actExit},
	}
	for _, c := range cases {
		if got := decideAgent(c.v, now); got != c.want {
			t.Errorf("%s: got %d want %d", c.name, got, c.want)
		}
	}
}

func TestAgentActivityKeepsFirstSince(t *testing.T) {
	var a agentActivity
	t0 := time.Date(2026, 9, 5, 12, 0, 0, 0, time.UTC)
	a.note("p", false, t0)
	a.note("p", false, t0.Add(time.Minute)) // still idle: since must not move
	if act, _ := a.get("p"); act.busy || !act.since.Equal(t0) {
		t.Fatalf("since moved: %+v", act)
	}
	a.note("p", true, t0.Add(2*time.Minute))
	if act, _ := a.get("p"); !act.busy || !act.since.Equal(t0.Add(2*time.Minute)) {
		t.Fatalf("busy flip not recorded: %+v", act)
	}
	a.forget("p")
	if _, ok := a.get("p"); ok {
		t.Fatal("forget did not remove")
	}
}

// The hooks encode attention for the lens: user_prompt_submit → idle is the
// moment an agent STARTS working; stop → done and permission → needs_input
// are when it stops. Getting this backwards ends sessions mid-task.
func TestClaudeBusyReadsHookAttentionCorrectly(t *testing.T) {
	if !claudeBusy(model.AttentionIdle) {
		t.Error("user_prompt_submit (attention idle) means the agent is working")
	}
	if claudeBusy(model.AttentionDone) || claudeBusy(model.AttentionNeedsInput) {
		t.Error("stop and permission prompts mean the agent is not working")
	}
}

func TestExitAgentSurvivesAVanishedPane(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "reg.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	m := New(context.Background(), &tmux.Server{Socket: "unused"}, st)
	m.exitAgent("no-such-pane", "no-such-ws") // must not panic
	if err := m.PushToAgentPane("no-such-pane", "x"); err != ErrNotAgentPane {
		t.Fatalf("push to unknown pane = %v, want ErrNotAgentPane", err)
	}
}

func TestWakeBackoffDoublesAndResets(t *testing.T) {
	var b wakeBackoff
	t0 := time.Date(2026, 9, 6, 9, 0, 0, 0, time.UTC)
	if !b.due("p", t0) {
		t.Fatal("first wake is due")
	}
	if w := b.failed("p", t0); w != 30*time.Second {
		t.Fatalf("first wait %s", w)
	}
	if b.due("p", t0.Add(10*time.Second)) {
		t.Fatal("not due inside the wait")
	}
	if w := b.failed("p", t0); w != time.Minute {
		t.Fatalf("second wait %s", w)
	}
	for i := 0; i < 12; i++ {
		b.failed("p", t0)
	}
	if w := b.failed("p", t0); w != time.Hour {
		t.Fatalf("capped wait %s", w)
	}
	b.forget("p")
	if !b.due("p", t0) {
		t.Fatal("forget resets the schedule")
	}
}

func TestStartMarksHoldForTheWindowThenLapse(t *testing.T) {
	var sm startMarks
	t0 := time.Date(2026, 9, 6, 9, 0, 0, 0, time.UTC)
	if sm.inProgress("p", t0) {
		t.Fatal("nothing marked yet")
	}
	sm.mark("p", t0)
	if !sm.inProgress("p", t0.Add(2*time.Second)) {
		t.Fatal("a start 2s ago is in progress")
	}
	if sm.inProgress("p", t0.Add(startWindow)) {
		t.Fatal("the window has lapsed")
	}
	sm.mark("p", t0)
	sm.forget("p")
	if sm.inProgress("p", t0) {
		t.Fatal("forget clears the mark")
	}
}

func TestWakeWithBackoff_InFlightStartIsNotAFailure(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "reg.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	m := New(context.Background(), &tmux.Server{Socket: "unused"}, st)
	t0 := time.Date(2026, 9, 6, 9, 0, 0, 0, time.UTC)
	p := model.Pane{ID: "p1", Agent: "x-poster"}
	m.WakeAgent = func(string, string) error { return ErrAgentRunning }
	m.wakeWithBackoff("w", p, t0)
	if !m.wakeBackoff.due("p1", t0) {
		t.Fatal("an in-flight start must not ratchet the backoff")
	}
	m.WakeAgent = func(string, string) error { return ErrAgentCap }
	m.wakeWithBackoff("w", p, t0)
	if m.wakeBackoff.due("p1", t0.Add(time.Second)) {
		t.Fatal("a real failure schedules a later retry")
	}
}

func TestStartMarks_PrunedOnDropAndConfirmedByTmux(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "reg.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	m := New(context.Background(), &tmux.Server{Socket: "unused"}, st)
	ws := &model.Workspace{ID: "w", Panes: []*model.Pane{{ID: "p0"}, {ID: "p1", Agent: "x-poster"}}}
	m.byID["w"] = &entry{ws: ws}
	now := time.Now()
	m.starting.mark(spawnKey("w", "x-poster"), now)
	m.starting.mark("p1", now)
	m.dropPane("w", "p1")
	if m.starting.inProgress(spawnKey("w", "x-poster"), now) || m.starting.inProgress("p1", now) {
		t.Fatal("dropping the agent's pane must prune both marks, so stop-then-add is not refused")
	}
	// tmux reporting the pane's foreground confirms a typed start.
	again := &model.Pane{ID: "p1", Agent: "x-poster"}
	ws.Panes = append(ws.Panes, again)
	m.starting.mark(spawnKey("w", "x-poster"), now)
	m.starting.mark("p1", now)
	m.starting.confirm("w", again)
	if m.starting.inProgress(spawnKey("w", "x-poster"), now) || m.starting.inProgress("p1", now) {
		t.Fatal("confirm must clear both marks")
	}
}
