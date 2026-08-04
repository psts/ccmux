package store

import (
	"path/filepath"
	"testing"
)

func taskStore(t *testing.T) *SQLite {
	t.Helper()
	st, err := Open(filepath.Join(t.TempDir(), "tasks.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

func TestPeerTasks_SaveGetUpsert(t *testing.T) {
	st := taskStore(t)
	if got, err := st.PeerTask("tsk_none"); err != nil || got != nil {
		t.Fatalf("missing task = %+v, %v; want nil, nil", got, err)
	}
	task := PeerTask{TaskID: "tsk_a", FromID: "d1", ToID: "w1", Group: "G",
		Text: "work", Status: "sent", CreatedAt: 100, UpdatedAt: 100}
	if err := st.SavePeerTask(task); err != nil {
		t.Fatal(err)
	}
	task.Status, task.Result, task.UpdatedAt = "completed", "done", 200
	if err := st.SavePeerTask(task); err != nil {
		t.Fatal(err)
	}
	got, err := st.PeerTask("tsk_a")
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != "completed" || got.Result != "done" || got.CreatedAt != 100 || got.UpdatedAt != 200 {
		t.Fatalf("after upsert = %+v", got)
	}
}

func TestPeerTasks_OpenListAndPrune(t *testing.T) {
	st := taskStore(t)
	rows := []PeerTask{
		{TaskID: "tsk_open", FromID: "d1", ToID: "w1", Status: "working", UpdatedAt: 10},
		{TaskID: "tsk_done_old", FromID: "d1", ToID: "w1", Status: "completed", UpdatedAt: 20},
		{TaskID: "tsk_done_new", FromID: "w1", ToID: "d1", Status: "failed", UpdatedAt: 900},
		{TaskID: "tsk_other", FromID: "x", ToID: "y", Status: "sent", UpdatedAt: 30},
	}
	for _, r := range rows {
		if err := st.SavePeerTask(r); err != nil {
			t.Fatal(err)
		}
	}

	// Open only, both directions, not other peers' tasks.
	open, err := st.OpenPeerTasksFor("w1", 20)
	if err != nil {
		t.Fatal(err)
	}
	if len(open) != 1 || open[0].TaskID != "tsk_open" {
		t.Fatalf("open for w1 = %+v", open)
	}

	// Prune removes only TERMINAL tasks older than the cutoff. The stale open
	// task must survive: unanswered delegations staying visible is the point.
	if err := st.PrunePeerTasks(500); err != nil {
		t.Fatal(err)
	}
	if got, _ := st.PeerTask("tsk_done_old"); got != nil {
		t.Fatal("old closed task survived prune")
	}
	for _, keep := range []string{"tsk_open", "tsk_done_new", "tsk_other"} {
		if got, _ := st.PeerTask(keep); got == nil {
			t.Fatalf("%s pruned, should be kept", keep)
		}
	}
}
