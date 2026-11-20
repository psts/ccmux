package manager

import (
	"testing"
	"time"
)

// A move restarts a tmux Claude only when it plainly sits idle: each other
// state is named, so the session told instead knows why.
func TestMoveBlocker_OnlyAnIdleClaudeIsRestarted(t *testing.T) {
	now := time.Now()
	idle := restartView{
		behind: true, eligible: true, runsClaude: true, known: true,
		clock:      restartClock{session: "abc", idleSince: now.Add(-time.Minute)},
		transcript: now.Add(-time.Minute),
	}
	if why := moveBlocker(idle, now); why != "" {
		t.Fatalf("an idle Claude was refused: %s", why)
	}
	cases := map[string]func(v *restartView){
		"no Claude runs in it":             func(v *restartView) { v.runsClaude = false },
		"its startup command is not one":   func(v *restartView) { v.eligible = false },
		"background task":                  func(v *restartView) { v.background = true },
		"a restart is already running":     func(v *restartView) { v.inflight = true },
		"working or waiting on a question": func(v *restartView) { v.clock.idleSince = time.Time{} },
		"waiting on a question (blocked)":  func(v *restartView) { v.clock.blocked = true },
		"active a moment ago":              func(v *restartView) { v.transcript = now.Add(-time.Second) },
		"its transcript was not found":     func(v *restartView) { v.transcript = time.Time{} },
		"no session known from the hooks":  func(v *restartView) { v.known = false },
	}
	for name, change := range cases {
		v := idle
		change(&v)
		if why := moveBlocker(v, now); why == "" {
			t.Errorf("%s: restarted anyway", name)
		}
	}
}

func TestTryBegin_OneRestartAtATime(t *testing.T) {
	var r autoRestart
	if !r.tryBegin("p1") {
		t.Fatal("the first begin was refused")
	}
	if r.tryBegin("p1") {
		t.Fatal("a second begin on the same pane went through")
	}
	if !r.tryBegin("p2") {
		t.Fatal("another pane was refused")
	}
	r.end("p1")
	if !r.tryBegin("p1") {
		t.Fatal("a pane whose restart ended was refused")
	}
}
