package manager

import (
	"testing"
	"time"
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
