package runner

import (
	"errors"
	"slices"
	"testing"

	"ccmux.dev/ccmuxd/internal/store"
)

// specConv is the conversation a session's spec starts its runner on.
func specConv(t *testing.T, r *rig, id string) string {
	t.Helper()
	sp, err := ReadSpec(r.paths.SpecFile(id))
	if err != nil {
		t.Fatal(err)
	}
	if i := slices.Index(sp.Args, "--session-auto"); i >= 0 && i+1 < len(sp.Args) {
		return sp.Args[i+1]
	}
	t.Fatalf("no --session-auto in %v", sp.Args)
	return ""
}

// A message that asks for a clean start moves its session onto a new
// conversation, lists the old one, and starts the runner again on the new
// one before the message is queued. Sent again, it opens no second one.
func TestACleanStartMovesTheSessionBeforeItsMessage(t *testing.T) {
	r := newRig(t)
	sess, _ := r.sup.Create(NewSession{Cwd: "/r/app"})
	r.reconcile()
	if _, err := r.sup.SendPrompt(sess.ID, Prompt{Text: "start over", Sender: "you", Clean: "k1"}); err != nil {
		t.Fatal(err)
	}
	row, _ := r.st.RunnerSession(sess.ID)
	if row.ConversationID() == sess.ID || len(row.Left) != 1 || row.Left[0].ID != sess.ID || row.Left[0].Key != "k1" {
		t.Fatalf("after a clean start: conv %q, left %+v", row.ConversationID(), row.Left)
	}
	if got := specConv(t, r, sess.ID); got != row.ConversationID() {
		t.Fatalf("the runner starts on %q, want the new conversation %q", got, row.ConversationID())
	}
	if n := r.units.count("restart"); n != 1 {
		t.Fatalf("restarts = %d, want 1: %v", n, r.units.calls)
	}
	if open, _ := r.st.UndeliveredPrompts(sess.ID); len(open) != 1 || open[0].Text != "start over" {
		t.Fatalf("the message waits for the new runner: %+v", open)
	}
	conv := row.ConversationID()
	if _, err := r.sup.SendPrompt(sess.ID, Prompt{Text: "start over", Sender: "you", Clean: "k1"}); err != nil {
		t.Fatal(err)
	}
	row, _ = r.st.RunnerSession(sess.ID)
	if row.ConversationID() != conv || len(row.Left) != 1 || r.units.count("restart") != 1 {
		t.Fatalf("a retry of the same message opened another: conv %q, left %+v, %v", row.ConversationID(), row.Left, r.units.calls)
	}
}

// A runner in the middle of a turn is not cut off for a clean start: the
// message is refused, saying why, and nothing moves.
func TestACleanStartWaitsForNoTurn(t *testing.T) {
	r := newRig(t)
	sess, _ := r.sup.Create(NewSession{Cwd: "/r/app"})
	r.reconcile()
	r.run[sess.ID].busy = true
	if _, err := r.sup.SendPrompt(sess.ID, Prompt{Text: "x", Clean: "k1"}); !errors.Is(err, ErrCleanRefused) {
		t.Fatalf("a busy runner: %v", err)
	}
	row, _ := r.st.RunnerSession(sess.ID)
	if row.ConversationID() != sess.ID || len(row.Left) != 0 || r.units.count("restart") != 0 {
		t.Fatalf("nothing moves: conv %q, left %+v, %v", row.ConversationID(), row.Left, r.units.calls)
	}
	if open, _ := r.st.UndeliveredPrompts(sess.ID); len(open) != 0 {
		t.Fatalf("the refused message is not queued: %+v", open)
	}
}

// An asleep session is not started just to be moved: the row moves, and
// the message wakes it onto the new conversation.
func TestACleanStartOfAnAsleepSessionMovesItsRowOnly(t *testing.T) {
	r := newRig(t)
	sess, _ := r.sup.Create(NewSession{Cwd: "/r/app"})
	r.reconcile()
	if err := r.sup.SetWanted(sess.ID, store.SessionAsleep); err != nil {
		t.Fatal(err)
	}
	r.reconcile()
	if _, err := r.sup.SendPrompt(sess.ID, Prompt{Text: "x", Clean: "k1"}); err != nil {
		t.Fatal(err)
	}
	if n := r.units.count("restart"); n != 0 {
		t.Fatalf("an asleep session was restarted: %v", r.units.calls)
	}
	r.reconcile()
	row, _ := r.st.RunnerSession(sess.ID)
	if row.Wanted != store.SessionRunning || specConv(t, r, sess.ID) != row.ConversationID() || row.ConversationID() == sess.ID {
		t.Fatalf("woken onto the new conversation: wanted %s, spec %q, row %q", row.Wanted, specConv(t, r, sess.ID), row.ConversationID())
	}
}

// A role agent says itself when it starts fresh: a clean start is refused.
func TestACleanStartIsRefusedForAnAgent(t *testing.T) {
	r := newRig(t)
	sess, _ := r.sup.Create(NewSession{Cwd: "/w/agents/radar", Kind: store.SessionAgent, WorkspaceID: "home"})
	r.reconcile()
	if _, err := r.sup.SendPrompt(sess.ID, Prompt{Text: "x", Clean: "k1"}); !errors.Is(err, ErrCleanRefused) {
		t.Fatalf("an agent: %v", err)
	}
}

// A clean start whose restart fails leaves the message waiting: the next
// pass restarts the runner on the new conversation before it delivers.
func TestACleanStartWhoseRestartFailedRestartsBeforeDelivering(t *testing.T) {
	r := newRig(t)
	sess, _ := r.sup.Create(NewSession{Cwd: "/r/app"})
	r.reconcile()
	old := r.run[sess.ID]
	failing := true
	r.units.onRestart = func(u string) {
		if !failing {
			r.bootRunner(u) // a restart that took starts a new runner
		}
	}
	r.units.mu.Lock()
	r.units.restartErr = errors.New("systemd busy")
	r.units.mu.Unlock()
	if _, err := r.sup.SendPrompt(sess.ID, Prompt{Text: "fresh", Clean: "k1"}); err != nil {
		t.Fatal(err)
	}
	r.units.mu.Lock()
	r.units.restartErr = nil
	r.units.mu.Unlock()
	failing = false
	r.reconcile()
	r.reconcile()
	row, _ := r.st.RunnerSession(sess.ID)
	now := r.run[sess.ID]
	if now == old || specConv(t, r, sess.ID) != row.ConversationID() {
		t.Fatalf("not restarted on the new conversation: spec %q, row %q, %v", specConv(t, r, sess.ID), row.ConversationID(), r.units.calls)
	}
	if len(old.sent) != 0 || len(now.sent) == 0 {
		t.Fatalf("the message went to the old runner (%v) instead of the new one (%v)", old.sent, now.sent)
	}
}
