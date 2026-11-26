package peers

import (
	"strings"
	"testing"
)

// The task runner's word to a task's filer reaches the repo's main session
// by the repo's plain name, asleep or not: a waking one as a prompt, once
// per fixed id.
func TestNotifyNamedWakesTheReposMainSessionOnce(t *testing.T) {
	b := newMainBus(t)
	b.haveMain("mainb", "backend", true)
	for i := 0; i < 2; i++ {
		if err := b.svc.NotifyNamed("SHOP", "backend", "queue-drained-w-2-backend", "the queue is empty", false); err != nil {
			t.Fatal(err)
		}
	}
	eventually(t, "the message in the main session's inbox", func() bool { return len(b.to("mainb")) == 1 })
	settle()
	if got := b.to("mainb"); len(got) != 1 || !strings.HasPrefix(got[0], "claude-peers|") || !strings.Contains(got[0], "the queue is empty") {
		t.Fatalf("the main session got %q, want the bus's one message", got)
	}
	if len(b.starts()) != 0 || b.hook.spawnCount() != 0 {
		t.Fatalf("it started something: %v, %d", b.starts(), b.hook.spawnCount())
	}
}

// A quiet one waits for the main session's next delivery and wakes nobody,
// and said again under its id, even after it was handed over, it is not
// stored again.
func TestNotifyNamedQuietWakesNobodyAndIsKeptOnce(t *testing.T) {
	b := newMainBus(t)
	main := b.haveMain("mainb", "backend", true)
	say := func() {
		if err := b.svc.NotifyNamed("SHOP", "backend", "task-landed-7", "Task #7 merged", true); err != nil {
			t.Fatal(err)
		}
	}
	say()
	say()
	b.svc.ReapOnce()
	settle()
	if got := b.to("mainb"); len(got) != 0 {
		t.Fatalf("a quiet notice woke the main session: %v", got)
	}
	if pending, err := b.svc.st.PendingNotices(main); err != nil || len(pending) != 1 {
		t.Fatalf("notices waiting = %v %v, want one", pending, err)
	}
	b.svc.Send(SendReq{FromID: b.app, ToName: "backend", Text: "a question"})
	eventually(t, "the next message", func() bool { return len(b.to("mainb")) == 1 })
	if got := b.to("mainb")[0]; !strings.Contains(got, "Task #7 merged") {
		t.Fatalf("the next message = %q, want the notice folded in", got)
	}
	say()
	if pending, _ := b.svc.st.PendingNotices(main); len(pending) != 0 {
		t.Fatalf("the notice was stored again after it was handed over: %v", pending)
	}
}

// Nobody to tell is an error, and nothing is started.
func TestNotifyNamedToNobodyStartsNothing(t *testing.T) {
	b := newMainBus(t)
	for _, quiet := range []bool{false, true} {
		if err := b.svc.NotifyNamed("SHOP", "backend", "", "hi", quiet); err == nil {
			t.Fatalf("quiet=%v: no main session, and no error", quiet)
		}
	}
	settle()
	if len(b.starts()) != 0 || b.hook.spawnCount() != 0 {
		t.Fatalf("it started something: %v, %d", b.starts(), b.hook.spawnCount())
	}
}
