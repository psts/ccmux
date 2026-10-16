package manager

import (
	"testing"
	"time"

	"ccmux.dev/ccmuxd/internal/model"
)

func TestRestartClockFollowsTheTurn(t *testing.T) {
	var c restartClocks
	t0 := time.Date(2026, 10, 16, 9, 0, 0, 0, time.UTC)
	c.session("p", "s1", model.SessionStarted)
	if got, _ := c.get("p"); got.session != "s1" || !got.idleSince.IsZero() {
		t.Fatalf("after start: %+v", got)
	}
	c.attention("p", model.AttentionIdle, model.ReasonNone, t0) // prompt submitted
	c.attention("p", model.AttentionDone, model.ReasonFinished, t0.Add(time.Minute))
	if got, _ := c.get("p"); !got.idleSince.Equal(t0.Add(time.Minute)) {
		t.Fatalf("a Stop must start the idle clock: %+v", got)
	}
	c.reminder("p", t0.Add(2*time.Minute))
	if got, _ := c.get("p"); !got.idleSince.Equal(t0.Add(time.Minute)) {
		t.Fatalf("the reminder must not move a running idle clock: %+v", got)
	}
	c.attention("p", model.AttentionIdle, model.ReasonNone, t0.Add(3*time.Minute))
	if got, _ := c.get("p"); !got.idleSince.IsZero() {
		t.Fatalf("a new prompt stops the clock: %+v", got)
	}
}

func TestRestartClockReminderStartsAFreshSession(t *testing.T) {
	var c restartClocks
	t0 := time.Date(2026, 10, 16, 9, 0, 0, 0, time.UTC)
	c.session("p", "s2", model.SessionStarted) // a /clear: no Stop will come
	c.reminder("p", t0)
	if got, _ := c.get("p"); !got.idleSince.Equal(t0) {
		t.Fatalf("idle_prompt after a start must start the clock: %+v", got)
	}
}

func TestRestartClockBlockedOnAQuestion(t *testing.T) {
	var c restartClocks
	t0 := time.Date(2026, 10, 16, 9, 0, 0, 0, time.UTC)
	c.attention("p", model.AttentionDone, model.ReasonFinished, t0)
	c.attention("p", model.AttentionNeedsInput, model.ReasonPermission, t0.Add(time.Minute))
	got, _ := c.get("p")
	if !got.blocked {
		t.Fatalf("a permission prompt must block: %+v", got)
	}
	c.reminder("p", t0.Add(2*time.Minute))
	if got, _ := c.get("p"); !got.blocked {
		t.Fatalf("a reminder must not unblock: %+v", got)
	}
	c.attention("p", model.AttentionIdle, model.ReasonNone, t0.Add(3*time.Minute))
	if got, _ := c.get("p"); got.blocked {
		t.Fatalf("an answer clears the block: %+v", got)
	}
}

func TestRestartClockSessionIDs(t *testing.T) {
	var c restartClocks
	c.session("p", "s1", model.SessionActive) // missed the start: heal from activity
	if got, _ := c.get("p"); got.session != "s1" {
		t.Fatalf("activity must fill an unknown session: %+v", got)
	}
	c.session("p", "s2", model.SessionStarted)
	c.session("p", "s1", model.SessionEnded) // the old one's end lands late
	if got, _ := c.get("p"); got.session != "s2" {
		t.Fatalf("a stale end must not clear the new session: %+v", got)
	}
	c.session("p", "s2", model.SessionEnded)
	if got, _ := c.get("p"); got.session != "" {
		t.Fatalf("its own end clears it: %+v", got)
	}
	c.session("p", "", model.SessionNone)
	c.forget("p")
	if _, ok := c.get("p"); ok {
		t.Fatal("forget must drop the pane")
	}
}

func TestDecideRestart(t *testing.T) {
	now := time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC)
	ready := restartView{
		behind: true, eligible: true, runsClaude: true, known: true,
		clock:      restartClock{session: "s", idleSince: now.Add(-61 * time.Minute)},
		transcript: now.Add(-61 * time.Minute),
	}
	with := func(f func(*restartView)) restartView { v := ready; f(&v); return v }
	cases := []struct {
		name string
		v    restartView
		want bool
	}{
		{"behind, idle an hour: restart", ready, true},
		{"up to date", with(func(v *restartView) { v.behind = false }), false},
		{"not a pane we relaunch", with(func(v *restartView) { v.eligible = false }), false},
		{"at a shell", with(func(v *restartView) { v.runsClaude = false }), false},
		{"no hooks seen", with(func(v *restartView) { v.known = false }), false},
		{"no session id", with(func(v *restartView) { v.clock.session = "" }), false},
		{"turn running", with(func(v *restartView) { v.clock.idleSince = time.Time{} }), false},
		{"idle 59 minutes", with(func(v *restartView) { v.clock.idleSince = now.Add(-59 * time.Minute) }), false},
		{"waiting on a card", with(func(v *restartView) { v.clock.blocked = true }), false},
		{"a background task running", with(func(v *restartView) { v.background = true }), false},
		{"failed on this version", with(func(v *restartView) { v.failed = true }), false},
		{"a transcript write within the hour (a turn no prompt hook announced)",
			with(func(v *restartView) { v.transcript = now.Add(-5 * time.Minute) }), false},
		{"a transcript quiet longer than the clock", with(func(v *restartView) { v.transcript = now.Add(-90 * time.Minute) }), true},
		{"no transcript found: never, however long the clock (a /compact or a resume, kept elsewhere)",
			with(func(v *restartView) { v.transcript = time.Time{}; v.clock.idleSince = now.Add(-48 * time.Hour) }), false},
		{"already restarting", with(func(v *restartView) { v.inflight = true }), false},
	}
	for _, c := range cases {
		if got := decideRestart(c.v, now, time.Hour); got != c.want {
			t.Errorf("%s: got %v want %v", c.name, got, c.want)
		}
	}
}

func TestRestartMark(t *testing.T) {
	ours := restartView{behind: true, eligible: true, runsClaude: true, known: true,
		clock: restartClock{session: "s"}, transcript: time.Now()}
	with := func(f func(*restartView)) restartView { v := ours; f(&v); return v }
	cases := []struct {
		name string
		v    restartView
		want string
	}{
		{"up to date", restartView{}, ""},
		{"behind, not ours: an agent or odd pane", restartView{behind: true}, ""},
		{"behind, ours, transcript found", ours, "pending"},
		{"session not known yet (daemon restarted)", with(func(v *restartView) { v.known, v.clock.session = false, ""; v.transcript = time.Time{} }), "pending"},
		{"known session, no transcript: cannot resume it", with(func(v *restartView) { v.transcript = time.Time{} }), "manual"},
		{"restart failed, still behind", with(func(v *restartView) { v.failed = true }), "manual"},
		{"restart failed, never came back", restartView{eligible: true, failed: true}, "manual"},
		{"restarted by hand onto the new one", restartView{eligible: true, failed: true, runsClaude: true}, ""},
	}
	for _, c := range cases {
		if got := restartMark(c.v); got != c.want {
			t.Errorf("%s: got %q want %q", c.name, got, c.want)
		}
	}
}
