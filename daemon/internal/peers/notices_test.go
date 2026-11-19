package peers

import "testing"

// A window notice reaches every session in that window from the bus
// itself, never the session it is about, and nobody in another window.
func TestNotifyGroup_ReachesTheWindowButNotItsSubject(t *testing.T) {
	svc, hook := newTestService(t)
	for pane, group := range map[string]string{"p-fe": "CL", "p-be": "CL", "p-admin": "CL", "p-x": "OTHER"} {
		hook.setGroup(pane, group)
	}
	fe := registerPane(svc, "p-fe", "/r/frontend")
	be := registerPane(svc, "p-be", "/r/backend")
	admin := registerPane(svc, "p-admin", "/r/admin")
	other := registerPane(svc, "p-x", "/r/elsewhere")

	if n := svc.NotifyGroup("CL", "admin", "admin joined this window"); n != 2 {
		t.Fatalf("reached %d sessions, want frontend and backend", n)
	}
	for _, id := range []string{fe.PeerID, be.PeerID} {
		evs, err := svc.Poll(id)
		if err != nil || len(evs) != 1 || evs[0].Text != "admin joined this window" || evs[0].FromName != "claude-peers" {
			t.Fatalf("%s got %v %v", id, evs, err)
		}
	}
	for _, id := range []string{admin.PeerID, other.PeerID} {
		if evs, _ := svc.Poll(id); len(evs) != 0 {
			t.Fatalf("%s must not hear it: %v", id, evs)
		}
	}
}

// A pane notice reaches only the session on that pane, from the bus itself;
// a pane with no session on the bus is an error, not a silent drop.
func TestNotifyPane_ReachesOnlyThatPane(t *testing.T) {
	svc, _ := newTestService(t)
	worker := registerPane(svc, "p-worker", "/r/app-task-1")
	other := registerPane(svc, "p-other", "/r/app")
	if err := svc.NotifyPane("p-worker", "the merge failed"); err != nil {
		t.Fatal(err)
	}
	evs, err := svc.Poll(worker.PeerID)
	if err != nil || len(evs) != 1 || evs[0].Text != "the merge failed" || evs[0].FromName != "claude-peers" {
		t.Fatalf("worker got %v %v", evs, err)
	}
	if evs, _ := svc.Poll(other.PeerID); len(evs) != 0 {
		t.Fatalf("another pane heard it: %v", evs)
	}
	if err := svc.NotifyPane("p-nobody", "hello"); err == nil {
		t.Fatal("a notice to a pane with no session was reported delivered")
	}
}

// A pane notice with a fixed id is stored and pushed once, however often it
// is said: the task runner's outbox says a notice again when it was cut off
// before it wrote down that it was said. Another id is another message, and
// a notice with no id is never taken for a repeat.
func TestNotifyPaneAs_TheSameIDIsHeardOnce(t *testing.T) {
	svc, _ := newTestService(t)
	worker := registerPane(svc, "p-worker", "/r/app-task-1")
	const id = "merge-task-1-ab12cd:part-1:send-back"
	for i := 0; i < 3; i++ {
		if err := svc.NotifyPaneAs("p-worker", id, "the merge failed"); err != nil {
			t.Fatalf("say %d: %v", i, err)
		}
	}
	evs, err := svc.Poll(worker.PeerID)
	if err != nil || len(evs) != 1 || evs[0].Text != "the merge failed" {
		t.Fatalf("said three times under one id, the worker has %v %v", evs, err)
	}
	if err := svc.NotifyPaneAs("p-worker", "merge-task-1-ef34ab:part-1:send-back", "it failed again"); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if err := svc.NotifyPane("p-worker", "report, please"); err != nil {
			t.Fatal(err)
		}
	}
	if evs, _ := svc.Poll(worker.PeerID); len(evs) != 3 {
		t.Fatalf("another id and two notices with none are three more messages: %v", evs)
	}
	// A notice nobody is there to hear is an error the first time; said
	// again after it was heard, it is no error even with the worker gone.
	if err := svc.NotifyPaneAs("p-nobody", "some-id", "hello"); err == nil {
		t.Fatal("a notice to a pane with no session was reported delivered")
	}
	if err := svc.NotifyPaneAs("p-nobody", id, "the merge failed"); err != nil {
		t.Fatalf("a notice that was heard is said again to nobody: %v", err)
	}
}
