package runner

import (
	"strings"
	"testing"
	"time"

	"ccmux.dev/ccmuxd/internal/store"
)

// stuckRig parks one session the way the start limit does, and returns
// the rig and the session.
func stuckRig(t *testing.T, n NewSession) (*rig, store.RunnerSession) {
	t.Helper()
	r := newRig(t)
	sess, err := r.sup.Create(n)
	if err != nil {
		t.Fatal(err)
	}
	if err := r.sup.park(sess.ID, "put to sleep: test"); err != nil {
		t.Fatal(err)
	}
	r.reconcile() // the live loop sees it within seconds
	return r, sess
}

func (r *rig) after(d time.Duration) {
	r.clock.mu.Lock()
	r.clock.at = r.clock.at.Add(d)
	r.clock.mu.Unlock()
}

// A person's session the daemon gave up starting tries again by itself,
// later each time: 10 minutes, then 30, then every hour.
func TestAStuckSessionTriesAgainByItselfLaterEachTime(t *testing.T) {
	r, sess := stuckRig(t, NewSession{Cwd: "/r/app"})
	r.reconcile()
	if got, _ := r.st.RunnerSession(sess.ID); got.Wanted != store.SessionAsleep {
		t.Fatalf("tried again at once: %+v", got)
	}
	r.after(11 * time.Minute)
	r.reconcile()
	got, _ := r.st.RunnerSession(sess.ID)
	if got.Wanted != store.SessionRunning || got.Stuck || !strings.Contains(got.Note, "by itself") {
		t.Fatalf("not woken after 10 minutes: %+v", got)
	}
	if err := r.sup.park(sess.ID, "put to sleep: again"); err != nil {
		t.Fatal(err)
	}
	r.reconcile() // the live loop sees it within seconds
	r.after(11 * time.Minute)
	r.reconcile()
	if got, _ := r.st.RunnerSession(sess.ID); got.Wanted != store.SessionAsleep {
		t.Fatalf("the second try came after 10 minutes, not 30: %+v", got)
	}
	r.after(20 * time.Minute)
	r.reconcile()
	if got, _ := r.st.RunnerSession(sess.ID); got.Wanted != store.SessionRunning {
		t.Fatalf("no second try after 30 minutes: %+v", got)
	}
}

// An agent sleeps between messages anyway: it tries again by itself only
// when a message waits for it. A task's builder is left to its queue.
func TestAStuckAgentTriesAgainOnlyForAWaitingMessage(t *testing.T) {
	r, sess := stuckRig(t, NewSession{Cwd: "/r/agent", Kind: store.SessionAgent})
	r.after(time.Hour)
	r.reconcile()
	if got, _ := r.st.RunnerSession(sess.ID); got.Wanted != store.SessionAsleep {
		t.Fatalf("an agent with nothing to do was woken: %+v", got)
	}
	if err := r.st.AddInboxPromptAndWake(store.InboxPrompt{ID: "p1", SessionID: sess.ID, Text: "post it", CreatedAt: 1}, false, 1); err != nil {
		t.Fatal(err)
	}
	r.reconcile()
	if got, _ := r.st.RunnerSession(sess.ID); got.Wanted != store.SessionRunning {
		t.Fatalf("an agent with a message waiting stayed stuck: %+v", got)
	}

	b, built := stuckRig(t, NewSession{Cwd: "/r/wt", Role: store.RoleBuilder})
	b.after(time.Hour)
	b.reconcile()
	if got, _ := b.st.RunnerSession(built.ID); got.Wanted != store.SessionAsleep {
		t.Fatalf("a builder was woken: %+v", got)
	}
}

// A session woken some other way (a person, a message) waits afresh when
// it is parked again: no burst of starts right after the park.
func TestAStuckSessionWokenByAPersonWaitsAfreshAtItsNextPark(t *testing.T) {
	r, sess := stuckRig(t, NewSession{Cwd: "/r/app"})
	r.after(5 * time.Minute)
	if err := r.sup.SetWanted(sess.ID, store.SessionRunning); err != nil {
		t.Fatal(err)
	}
	r.reconcile()
	r.after(48 * time.Hour)
	if err := r.sup.park(sess.ID, "put to sleep: again"); err != nil {
		t.Fatal(err)
	}
	r.reconcile()
	if got, _ := r.st.RunnerSession(sess.ID); got.Wanted != store.SessionAsleep {
		t.Fatalf("retried at once after a later park: %+v", got)
	}
	r.after(11 * time.Minute)
	r.reconcile()
	if got, _ := r.st.RunnerSession(sess.ID); got.Wanted != store.SessionRunning {
		t.Fatalf("not retried 10 minutes after the later park: %+v", got)
	}
}
