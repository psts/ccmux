package runner

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

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
	fr.rewindTo = Rewind{Dropped: 1, Conversation: "3f2c1b0a-9e8d-4c7b-a6f5-e4d3c2b1a090"}
	if _, err := r.sup.Rewind(context.Background(), sess.ID, old, false); err != nil {
		t.Fatal(err)
	}
	row, _ := r.st.RunnerSession(sess.ID)
	if row.ConversationID() != "3f2c1b0a-9e8d-4c7b-a6f5-e4d3c2b1a090" {
		t.Fatalf("the fork is the session's conversation now: %s", row.ConversationID())
	}
	if r.units.count("restart "+r.paths.Unit(sess.ID)) != 1 {
		t.Fatalf("started again once: %v", r.units.calls)
	}
	sp, _ := ReadSpec(r.paths.SpecFile(sess.ID))
	if !strings.Contains(strings.Join(sp.Args, " "), "--session-auto 3f2c1b0a-9e8d-4c7b-a6f5-e4d3c2b1a090") {
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

// A runner that went back takes no prompt until it starts again, so a
// restart that fails after it is tried again at the next pass, on the fork.
func TestRewindThatCouldNotRestartIsRestartedAtTheNextPass(t *testing.T) {
	r := newRig(t)
	sess, _ := r.sup.Create(NewSession{Cwd: "/r/app"})
	r.reconcile()
	r.run[sess.ID].rewindTo = Rewind{Dropped: 1, Conversation: "0b9c6a1e-5d6f-4a6c-9b3e-2f1d8c7a6b5e"}
	r.units.restartErr = errors.New("systemd busy")
	_, err := r.sup.Rewind(context.Background(), sess.ID, "p1", false)
	if err == nil || !strings.Contains(err.Error(), "starts again in a moment") {
		t.Fatalf("the failure is said: %v", err)
	}
	r.units.restartErr = nil
	before := r.units.count("restart")
	r.reconcile()
	if r.units.count("restart") != before+1 {
		t.Fatalf("restarted at the next pass: %v", r.units.calls)
	}
	sp, _ := ReadSpec(r.paths.SpecFile(sess.ID))
	if !strings.Contains(strings.Join(sp.Args, " "), "--session-auto 0b9c6a1e-5d6f-4a6c-9b3e-2f1d8c7a6b5e") {
		t.Fatalf("on the fork: %v", sp.Args)
	}
}

// A conversation that is not an id is never saved (it goes into a path);
// the runner is still restarted, on the conversation it left.
func TestRewindRefusesAConversationThatIsNotAnID(t *testing.T) {
	r := newRig(t)
	sess, _ := r.sup.Create(NewSession{Cwd: "/r/app"})
	r.reconcile()
	r.run[sess.ID].rewindTo = Rewind{Dropped: 1, Conversation: "../../x"}
	if _, err := r.sup.Rewind(context.Background(), sess.ID, "p1", false); err == nil || !strings.Contains(err.Error(), "is not an id") {
		t.Fatalf("refused: %v", err)
	}
	if row, _ := r.st.RunnerSession(sess.ID); row.ConversationID() != sess.ID {
		t.Fatalf("not saved: %s", row.ConversationID())
	}
	before := r.units.count("restart")
	r.reconcile()
	if r.units.count("restart") != before+1 {
		t.Fatalf("restarted on the conversation it left, so it takes prompts again: %v", r.units.calls)
	}
}

// A failure that is not a refusal may come after the runner went back
// (its answer lost), so the session is restarted; a refusal restarts nothing.
func TestRewindFailureRestartsTheSessionButARefusalDoesNot(t *testing.T) {
	r := newRig(t)
	sess, _ := r.sup.Create(NewSession{Cwd: "/r/app"})
	r.reconcile()
	fr := r.run[sess.ID]
	fr.rewindNo = "Claude is working; stop the turn first"
	r.sup.Rewind(context.Background(), sess.ID, "p1", false)
	r.reconcile()
	if r.units.count("restart") != 0 {
		t.Fatalf("a refusal restarts nothing: %v", r.units.calls)
	}
	fr.srv.Close() // the runner's answer is lost
	if _, err := r.sup.Rewind(context.Background(), sess.ID, "p1", false); err == nil || !strings.Contains(err.Error(), "may have put files back") {
		t.Fatalf("a lost answer is a failure that warns about the files: %v", err)
	}
	r.bootRunner(r.paths.Unit(sess.ID)) // it is still up, between turns
	r.reconcile()
	if r.units.count("restart") != 1 {
		t.Fatalf("restarted at the next pass: %v", r.units.calls)
	}
}

// A runner that tried and says it failed answered, and takes prompts again:
// its words reach the person, and nothing is restarted.
func TestRewindFailureTheRunnerReportsIsSaidInItsWordsAndRestartsNothing(t *testing.T) {
	r := newRig(t)
	sess, _ := r.sup.Create(NewSession{Cwd: "/r/app"})
	r.reconcile()
	r.run[sess.ID].rewindFail = "the files were put back, but the conversation could not be cut: disk full"
	_, err := r.sup.Rewind(context.Background(), sess.ID, "p1", false)
	if !errors.Is(err, ErrRewindFailed) || !strings.HasSuffix(err.Error(), ": the files were put back, but the conversation could not be cut: disk full") || strings.Contains(err.Error(), "may have") {
		t.Fatalf("in the runner's words: %v", err)
	}
	r.reconcile()
	if r.units.count("restart") != 0 {
		t.Fatalf("it answered, so nothing restarts: %v", r.units.calls)
	}
}

// The caller leaving does not cut the runner's answer short: the fork is
// still saved and the session started on it.
func TestRewindLandsEvenWhenTheCallerLeaves(t *testing.T) {
	r := newRig(t)
	sess, _ := r.sup.Create(NewSession{Cwd: "/r/app"})
	r.reconcile()
	fr := r.run[sess.ID]
	fr.rewindTo = Rewind{Dropped: 1, Conversation: "7d1e2f3a-4b5c-4d6e-8f70-81a2b3c4d5e6"}
	fr.rewindWait = 300 * time.Millisecond
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if _, err := r.sup.Rewind(ctx, sess.ID, "p1", false); err != nil {
		t.Fatalf("the answer came after the caller left, and was still taken: %v", err)
	}
	if row, _ := r.st.RunnerSession(sess.ID); row.ConversationID() != "7d1e2f3a-4b5c-4d6e-8f70-81a2b3c4d5e6" {
		t.Fatalf("on the fork: %s", row.ConversationID())
	}
	if r.units.count("restart") != 1 {
		t.Fatalf("started again once: %v", r.units.calls)
	}
}

// A real going back outlasts the client's usual bound and still lands; a
// look keeps that bound.
func TestRewindOutlastsTheUsualClientBoundButALookDoesNot(t *testing.T) {
	r := newRig(t)
	sess, _ := r.sup.Create(NewSession{Cwd: "/r/app"})
	r.reconcile()
	orig := r.sup.dial
	r.sup.dial = func(socket string) *Client {
		c := orig(socket)
		c.http.Timeout = 100 * time.Millisecond // stands in for the usual 15 s
		return c
	}
	fr := r.run[sess.ID]
	fr.rewindTo = Rewind{Dropped: 1, Conversation: "5a6b7c8d-9e0f-4a1b-8c2d-3e4f5a6b7c8d"}
	fr.rewindWait = 300 * time.Millisecond
	if _, err := r.sup.Rewind(context.Background(), sess.ID, "p1", true); err == nil {
		t.Fatal("a look past the usual bound times out")
	}
	if _, err := r.sup.Rewind(context.Background(), sess.ID, "p1", false); err != nil {
		t.Fatalf("a real going back gets its own, longer bound: %v", err)
	}
	if row, _ := r.st.RunnerSession(sess.ID); row.ConversationID() != "5a6b7c8d-9e0f-4a1b-8c2d-3e4f5a6b7c8d" {
		t.Fatalf("on the fork: %s", row.ConversationID())
	}
}
