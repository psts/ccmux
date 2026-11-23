package runner

import (
	"context"
	"errors"
	"net"
	"net/http"
	"path/filepath"
	"slices"
	"sync"
	"testing"
	"time"

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

// A runner that does not answer in time may be mid-turn: refused. One with
// no socket is down, and has no turn to cut: the clean start goes ahead.
func TestACleanStartRefusesARunnerThatDoesNotAnswer(t *testing.T) {
	r := newRig(t)
	sess, _ := r.sup.Create(NewSession{Cwd: "/r/app"})
	r.reconcile()
	r.sup.dial = func(string) *Client {
		return &Client{http: &http.Client{Transport: failingTransport{context.DeadlineExceeded}}}
	}
	if _, err := r.sup.SendPrompt(sess.ID, Prompt{Text: "x", Clean: "k1"}); !errors.Is(err, ErrCleanRefused) {
		t.Fatalf("a runner that did not answer: %v", err)
	}
	if row, _ := r.st.RunnerSession(sess.ID); row.ConversationID() != sess.ID {
		t.Fatalf("nothing moves: %q", row.ConversationID())
	}
	r.sup.dial = func(string) *Client { return Dial(filepath.Join(t.TempDir(), "none.sock")) }
	if _, err := r.sup.SendPrompt(sess.ID, Prompt{Text: "x", Clean: "k2"}); err != nil {
		t.Fatalf("a runner with no socket is down, not busy: %v", err)
	}
	if row, _ := r.st.RunnerSession(sess.ID); row.ConversationID() == sess.ID {
		t.Fatal("the clean start was made")
	}
	moved, _ := r.st.RunnerSession(sess.ID)
	stale := staleSocket(t) // a crashed runner leaves its socket file behind
	r.sup.dial = func(string) *Client { return Dial(stale) }
	if _, err := r.sup.SendPrompt(sess.ID, Prompt{Text: "x", Clean: "k3"}); err != nil {
		t.Fatalf("a socket nobody listens on is down, not busy: %v", err)
	}
	if row, _ := r.st.RunnerSession(sess.ID); row.ConversationID() == moved.ConversationID() {
		t.Fatal("the clean start past a stale socket was made")
	}
}

// staleSocket is a unix socket file with no listener behind it.
func staleSocket(t *testing.T) string {
	path := filepath.Join(t.TempDir(), "stale.sock")
	l, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	l.(*net.UnixListener).SetUnlinkOnClose(false)
	l.Close()
	return path
}

type failingTransport struct{ err error }

func (f failingTransport) RoundTrip(*http.Request) (*http.Response, error) { return nil, f.err }

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

// Start over moves a session onto a new conversation with no message: the
// old one is listed, the runner starts again on the new one, and nothing
// is queued. Asked twice, it starts over twice. A turn in progress, an
// agent or an ended session is refused, and nothing moves.
func TestStartOverMovesTheSessionWithNoMessage(t *testing.T) {
	r := newRig(t)
	sess, _ := r.sup.Create(NewSession{Cwd: "/r/app"})
	r.reconcile()
	if err := r.sup.StartOver(sess.ID); err != nil {
		t.Fatal(err)
	}
	row, _ := r.st.RunnerSession(sess.ID)
	first := row.ConversationID()
	if first == sess.ID || len(row.Left) != 1 || row.Left[0].ID != sess.ID || specConv(t, r, sess.ID) != first || r.units.count("restart") != 1 {
		t.Fatalf("after start over: conv %q, left %+v, spec %q, %v", first, row.Left, specConv(t, r, sess.ID), r.units.calls)
	}
	if open, _ := r.st.UndeliveredPrompts(sess.ID); len(open) != 0 {
		t.Fatalf("no message is queued: %+v", open)
	}
	if err := r.sup.StartOver(sess.ID); err != nil {
		t.Fatal(err)
	}
	if row, _ = r.st.RunnerSession(sess.ID); row.ConversationID() == first || len(row.Left) != 2 {
		t.Fatalf("a second start over is a second new conversation: conv %q, left %+v", row.ConversationID(), row.Left)
	}

	r.run[sess.ID].busy = true
	before := row.ConversationID()
	if err := r.sup.StartOver(sess.ID); !errors.Is(err, ErrCleanRefused) {
		t.Fatalf("in a turn: %v", err)
	}
	if row, _ = r.st.RunnerSession(sess.ID); row.ConversationID() != before {
		t.Fatal("a refused start over moved the session")
	}
	agent, _ := r.sup.Create(NewSession{Cwd: "/w/agents/radar", Kind: store.SessionAgent, WorkspaceID: "home"})
	if err := r.sup.StartOver(agent.ID); !errors.Is(err, ErrCleanRefused) {
		t.Fatalf("an agent: %v", err)
	}
	if err := r.sup.SetWanted(sess.ID, store.SessionDone); err != nil {
		t.Fatal(err)
	}
	if err := r.sup.StartOver(sess.ID); !errors.Is(err, ErrSessionDone) {
		t.Fatalf("an ended session: %v", err)
	}
}

// CleanStarted hears each new conversation that was made, once: a clean
// start and a Start over, never one refused or a message sent again under
// its key.
func TestCleanStartedHearsOnlyAStartThatHappened(t *testing.T) {
	r := newRig(t)
	sess, _ := r.sup.Create(NewSession{Cwd: "/r/app"})
	r.reconcile()
	var mu sync.Mutex
	var heard []string
	r.sup.CleanStarted = func(s store.RunnerSession) {
		mu.Lock()
		defer mu.Unlock()
		heard = append(heard, s.ConversationID())
	}
	got := func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), heard...)
	}
	if _, err := r.sup.SendPrompt(sess.ID, Prompt{Text: "start over", Clean: "k1"}); err != nil {
		t.Fatal(err)
	}
	if _, err := r.sup.SendPrompt(sess.ID, Prompt{Text: "start over", Clean: "k1", ID: "again"}); err != nil {
		t.Fatal(err)
	}
	if _, err := r.sup.SendPrompt(sess.ID, Prompt{Text: "no clean start"}); err != nil {
		t.Fatal(err)
	}
	r.run[sess.ID].busy = true
	if _, err := r.sup.SendPrompt(sess.ID, Prompt{Text: "x", Clean: "k2"}); !errors.Is(err, ErrCleanRefused) {
		t.Fatalf("busy: %v", err)
	}
	if err := r.sup.StartOver(sess.ID); !errors.Is(err, ErrCleanRefused) {
		t.Fatalf("busy start over: %v", err)
	}
	r.run[sess.ID].busy = false
	if err := r.sup.StartOver(sess.ID); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "both starts heard", func() bool { return len(got()) >= 2 })
	time.Sleep(50 * time.Millisecond) // nothing else is heard late
	row, _ := r.st.RunnerSession(sess.ID)
	h := got()
	if len(h) != 2 || h[0] == h[1] || (h[0] != row.ConversationID() && h[1] != row.ConversationID()) {
		t.Fatalf("heard %v, want the clean start's and the start over's conversations", h)
	}
}

// What CleanStarted does may take a lock that someone else holds while it
// messages the same session (a person's stand-down holds its side thread's
// lock and tells the main session). With the runner's real locks, that
// must not deadlock: the start returns, and so does the message.
func TestCleanStartedRunsOffTheSessionsLock(t *testing.T) {
	r := newRig(t)
	sess, _ := r.sup.Create(NewSession{Cwd: "/r/app"})
	r.reconcile()
	var thread sync.Mutex // stands in for the side thread's lock
	entered := make(chan struct{})
	var once sync.Once
	r.sup.CleanStarted = func(store.RunnerSession) {
		once.Do(func() { close(entered) })
		thread.Lock()
		thread.Unlock()
	}
	thread.Lock() // the person's stand-down holds the thread
	started := make(chan error, 1)
	go func() { started <- r.sup.StartOver(sess.ID) }()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("the start never reached its hook")
	}
	sent := make(chan error, 1)
	go func() { _, err := r.sup.Send(sess.ID, "[ccmux] a person stood a helper down", "ccmux"); sent <- err }()
	for name, ch := range map[string]chan error{"the message to the session": sent, "the start over": started} {
		select {
		case err := <-ch:
			if err != nil {
				t.Fatalf("%s: %v", name, err)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("%s never returned: the session's lock is held across its hook", name)
		}
	}
	thread.Unlock()
}

// waitFor waits for something a goroutine does.
func waitFor(t *testing.T, what string, ok func() bool) {
	t.Helper()
	for end := time.Now().Add(5 * time.Second); time.Now().Before(end); time.Sleep(2 * time.Millisecond) {
		if ok() {
			return
		}
	}
	t.Fatalf("never happened: %s", what)
}
