package taskq

import (
	"strings"
	"testing"

	"ccmux.dev/ccmuxd/internal/store"
)

func list(ts ...store.Task) []store.Task { return ts }

func TestCheckAfter(t *testing.T) {
	all := list(
		store.Task{ID: 1, Status: Waiting},
		store.Task{ID: 2, Status: Waiting, AfterID: 1},
		store.Task{ID: 3, Status: Cancelled},
		store.Task{ID: 4, Status: Done},
	)
	cases := []struct {
		name      string
		id, after int64
		wantErr   string
	}{
		{"none", 5, 0, ""},
		{"new task after a waiting one", 0, 2, ""},
		{"after a done task", 0, 4, ""},
		{"missing", 0, 9, "no task #9"},
		{"cancelled", 0, 3, "cancelled"},
		{"itself", 1, 1, "loop"},
		{"two-step loop", 1, 2, "loop"},
	}
	for _, c := range cases {
		err := CheckAfter(all, c.id, c.after)
		if c.wantErr == "" && err != nil {
			t.Errorf("%s: unexpected %v", c.name, err)
		}
		if c.wantErr != "" && (err == nil || !strings.Contains(err.Error(), c.wantErr)) {
			t.Errorf("%s: err = %v, want it to mention %q", c.name, err, c.wantErr)
		}
	}
}

func TestCheckAfter_StopsOnACorruptLoop(t *testing.T) {
	// Rows written before the check existed could already loop; the walk
	// must end rather than spin.
	all := list(store.Task{ID: 1, AfterID: 2}, store.Task{ID: 2, AfterID: 1})
	if err := CheckAfter(all, 0, 1); err == nil {
		t.Fatal("a stored loop was accepted")
	}
}

func TestCanMove(t *testing.T) {
	if !CanMove(Planning, Waiting) || !CanMove(Waiting, Planning) || !CanMove(Failed, Waiting) {
		t.Fatal("a human move was refused")
	}
	for _, to := range []string{Done, Planning, Cancelled, Waiting} {
		if CanMove(Running, to) {
			t.Fatalf("a running task may not be moved to %s by hand", to)
		}
	}
	if CanMove(Waiting, Done) {
		t.Fatal("only the runner finishes a task")
	}
}

func TestApply(t *testing.T) {
	task := store.Task{ID: 1, Title: "t", Plan: "p", Status: Waiting}
	plan := "new"
	got, err := Apply(task, list(task), Edit{Plan: &plan})
	if err != nil || got.Plan != "new" {
		t.Fatalf("plan edit = %+v, %v", got, err)
	}
	running := task
	running.Status = Running
	if _, err := Apply(running, list(running), Edit{Plan: &plan}); err == nil {
		t.Fatal("a running task's plan was edited")
	}
	cancel := Cancelled
	if _, err := Apply(running, list(running), Edit{Status: &cancel}); err == nil {
		t.Fatal("a running task was cancelled by hand")
	}
	empty := "  "
	if _, err := Apply(task, list(task), Edit{Title: &empty}); err == nil {
		t.Fatal("a blank title was accepted")
	}
	same := Waiting
	if got, err := Apply(task, list(task), Edit{Status: &same}); err != nil || got.Status != Waiting {
		t.Fatalf("a no-op status = %+v, %v", got, err)
	}
}

func TestCounts(t *testing.T) {
	c := Counts(list(store.Task{Status: Waiting}, store.Task{Status: Waiting}, store.Task{Status: Done}))
	if c[Waiting] != 2 || c[Done] != 1 || c[Running] != 0 {
		t.Fatalf("counts = %v", c)
	}
	if len(c) != len(Statuses) {
		t.Fatalf("counts has %d keys, want every status", len(c))
	}
}

func TestNeighbour_SkipsOtherStatuses(t *testing.T) {
	all := list(
		store.Task{ID: 1, Status: Waiting},
		store.Task{ID: 2, Status: Planning},
		store.Task{ID: 3, Status: Waiting},
	)
	if got, ok := Neighbour(all, 3, true); !ok || got.ID != 1 {
		t.Fatalf("up from 3 = %d %v, want 1 (the planning task is in another list)", got.ID, ok)
	}
	if _, ok := Neighbour(all, 3, false); ok {
		t.Fatal("down from the last waiting task found something")
	}
	if _, ok := Neighbour(all, 9, true); ok {
		t.Fatal("a missing task found a neighbour")
	}
}

// A task put back in the queue is checked against what it starts after,
// which may have been cancelled while it sat out.
func TestApply_RequeueRechecksAfter(t *testing.T) {
	dep := store.Task{ID: 1, Status: Cancelled}
	task := store.Task{ID: 2, Title: "t", Plan: "p", Status: Cancelled, AfterID: 1}
	waiting := Waiting
	if _, err := Apply(task, list(dep, task), Edit{Status: &waiting}); err == nil || !strings.Contains(err.Error(), "change what it starts after") {
		t.Fatalf("requeue after a cancelled task = %v", err)
	}
	none := int64(0)
	task.Note = "Claude exited before it reported"
	got, err := Apply(task, list(dep, task), Edit{Status: &waiting, AfterID: &none})
	if err != nil || got.Status != Waiting || got.AfterID != 0 || got.Note != "" {
		t.Fatalf("requeue that clears after = %+v, %v", got, err)
	}
}
