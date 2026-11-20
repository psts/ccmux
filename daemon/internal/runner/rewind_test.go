package runner

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	"ccmux.dev/ccmuxd/internal/store"
)

func TestRewindLooksWithoutChangingAnything(t *testing.T) {
	r := newRig(t)
	sess, _ := r.sup.Create(NewSession{Cwd: "/r/app"})
	r.reconcile()
	fr := r.run[sess.ID]
	fr.rewindTo = Rewind{Dropped: 2, Files: RewindFiles{CanRewind: true, Files: []string{"/r/app/b.txt"}}, Conversation: "fork-1"}
	got, err := r.sup.Rewind(context.Background(), sess.ID, "p2", true)
	if err != nil {
		t.Fatal(err)
	}
	if got.Dropped != 2 || got.Conversation != "" || !slices.Equal(got.Files.Files, []string{"/r/app/b.txt"}) {
		t.Fatalf("the look: %+v", got)
	}
	row, _ := r.st.RunnerSession(sess.ID)
	if row.ConversationID() != sess.ID || r.units.count("restart") != 0 {
		t.Fatalf("a look moves nothing: conversation %s, %v", row.ConversationID(), r.units.calls)
	}
}

func TestRewindStartsTheSessionAgainOnTheFork(t *testing.T) {
	r := newRig(t)
	sess, _ := r.sup.Create(NewSession{Cwd: "/r/app"})
	old, _ := r.sup.Send(sess.ID, "the prompt being taken back", "you")
	r.reconcile()
	fr := r.run[sess.ID]
	fr.transcript[old] = true // Claude has it, but the inbox has not marked it yet
	fr.rewindTo = Rewind{Dropped: 1, Conversation: "fork-1"}
	if _, err := r.sup.Rewind(context.Background(), sess.ID, old, false); err != nil {
		t.Fatal(err)
	}
	row, _ := r.st.RunnerSession(sess.ID)
	if row.ConversationID() != "fork-1" {
		t.Fatalf("the fork is the session's conversation now: %s", row.ConversationID())
	}
	if r.units.count("restart "+r.paths.Unit(sess.ID)) != 1 {
		t.Fatalf("started again once: %v", r.units.calls)
	}
	sp, _ := ReadSpec(r.paths.SpecFile(sess.ID))
	if !strings.Contains(strings.Join(sp.Args, " "), "--session-auto fork-1") {
		t.Fatalf("on the fork: %v", sp.Args)
	}
	if open, _ := r.st.UndeliveredPrompts(sess.ID); len(open) != 0 {
		t.Fatalf("the prompt taken back is never sent to the fork: %+v", open)
	}
}

func TestRewindRefusalsSayWhy(t *testing.T) {
	r := newRig(t)
	sess, _ := r.sup.Create(NewSession{Cwd: "/r/app"})
	r.reconcile()
	r.run[sess.ID].rewindNo = "Claude is working; stop the turn first"
	_, err := r.sup.Rewind(context.Background(), sess.ID, "p1", false)
	if !errors.Is(err, ErrRewindRefused) || !strings.Contains(err.Error(), "stop the turn first") {
		t.Fatalf("the runner's words: %v", err)
	}
	if row, _ := r.st.RunnerSession(sess.ID); row.ConversationID() != sess.ID || r.units.count("restart") != 0 {
		t.Fatalf("a refusal moves nothing: %v", r.units.calls)
	}
	if err := r.sup.SetWanted(sess.ID, store.SessionAsleep); err != nil {
		t.Fatal(err)
	}
	if _, err := r.sup.Rewind(context.Background(), sess.ID, "p1", true); !errors.Is(err, ErrRewindRefused) {
		t.Fatalf("an asleep session has no runner to ask: %v", err)
	}
}
