package runner

import (
	"testing"
	"time"

	"ccmux.dev/ccmuxd/internal/store"
)

// Cold and Finished ask the runner itself, over its socket: a session that answered
// and whose cache has run out is cold; one at work, warm, holding a card,
// or not answering is not.
func TestColdAsksTheRunner(t *testing.T) {
	ended := func(ago time.Duration) map[string]any {
		return map[string]any{"result": map[string]any{"endedAt": time.Now().Add(-ago).UnixMilli(), "cacheTtlMs": 3600000}}
	}
	for name, c := range map[string]struct {
		busy, turnOpen bool
		snap           map[string]any
		want, finished bool
	}{
		"cold":                  {snap: ended(61 * time.Minute), want: true, finished: true},
		"warm":                  {snap: ended(10 * time.Minute), finished: true},
		"in a turn":             {busy: true, snap: ended(2 * time.Hour)},
		"a turn open":           {turnOpen: true, snap: ended(2 * time.Hour)},
		"never finished a turn": {snap: map[string]any{}},
		"a card":                {snap: map[string]any{"questions": []any{map[string]any{"id": "q1"}}, "result": map[string]any{"endedAt": 1}}},
	} {
		t.Run(name, func(t *testing.T) {
			r := newRig(t)
			r.clock.at = time.Now()
			sess, err := r.sup.Create(NewSession{Cwd: "/r/app"})
			if err != nil {
				t.Fatal(err)
			}
			r.reconcile()
			fr := r.run[sess.ID]
			if fr == nil {
				t.Fatal("no runner on the rig")
			}
			fr.busy, fr.turnOpen, fr.snap = c.busy, c.turnOpen, c.snap
			if got := r.sup.Cold(sess.ID); got != c.want {
				t.Fatalf("cold: got %v", got)
			}
			if got := r.sup.Finished(sess.ID); got != c.finished {
				t.Fatalf("finished: got %v", got)
			}
		})
	}
}

// An ended session is not cold (there is nothing to end), and an asleep one
// is, once nothing waits on it.
func TestColdOfEndedAndAsleepSessions(t *testing.T) {
	r := newRig(t)
	sess, err := r.sup.Create(NewSession{Cwd: "/r/app"})
	if err != nil {
		t.Fatal(err)
	}
	if err := r.sup.SetWanted(sess.ID, store.SessionAsleep); err != nil {
		t.Fatal(err)
	}
	if !r.sup.Cold(sess.ID) {
		t.Fatal("an asleep session with nothing waiting is cold")
	}
	if err := r.sup.SetWanted(sess.ID, store.SessionDone); err != nil {
		t.Fatal(err)
	}
	if r.sup.Cold(sess.ID) {
		t.Fatal("an ended session is not cold")
	}
}

// A running session whose runner does not answer (restarting, a socket that
// is gone) is not cold: ending it could cut what it is doing.
func TestARunnerThatDoesNotAnswerIsNotCold(t *testing.T) {
	r := newRig(t)
	sess, err := r.sup.Create(NewSession{Cwd: "/r/app"})
	if err != nil {
		t.Fatal(err)
	}
	if r.sup.Cold(sess.ID) {
		t.Fatal("no runner has answered for it, so it is not cold")
	}
}
