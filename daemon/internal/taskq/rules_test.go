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
	all := list(store.Task{ID: 1, AfterID: 2}, store.Task{ID: 2, AfterID: 1})
	if err := CheckAfter(all, 0, 1); err == nil {
		t.Fatal("a stored loop was accepted")
	}
}

func TestCanMove(t *testing.T) {
	for _, m := range [][2]string{{Planning, Waiting}, {Waiting, Planning}, {Running, Cancelled}, {NeedsYou, Cancelled}, {ReadyToMerge, Done}, {Cancelled, Waiting}, {MergeFailed, ReadyToMerge}, {MergeFailed, Done}} {
		if !CanMove(m[0], m[1]) {
			t.Errorf("%s -> %s refused", m[0], m[1])
		}
	}
	for _, m := range [][2]string{{Waiting, Done}, {Running, Waiting}, {Running, Done}, {Done, Waiting}, {Merging, Done}, {Merging, Cancelled}, {ReadyToMerge, Merging}} {
		if CanMove(m[0], m[1]) {
			t.Errorf("%s -> %s allowed by hand", m[0], m[1])
		}
	}
}

func TestApply(t *testing.T) {
	task := store.Task{ID: 1, Title: "t", Status: Waiting}
	title := " new "
	got, err := Apply(task, list(task), Edit{Title: &title})
	if err != nil || got.Title != "new" {
		t.Fatalf("title edit = %+v, %v", got, err)
	}
	running := task
	running.Status = Running
	if _, err := Apply(running, list(running), Edit{Title: &title}); err == nil {
		t.Fatal("a running task's title was edited")
	}
	blank := "  "
	if _, err := Apply(task, list(task), Edit{Title: &blank}); err == nil {
		t.Fatal("a blank title was accepted")
	}
	cancel := Cancelled
	if got, err := Apply(running, list(running), Edit{Status: &cancel}); err != nil || got.Status != Cancelled {
		t.Fatalf("cancel a running task = %+v, %v", got, err)
	}
}

// A task put back in the queue is checked against what it starts after,
// which may have been cancelled while it sat out, and loses its old note.
func TestApply_RequeueRechecksAfter(t *testing.T) {
	dep := store.Task{ID: 1, Status: Cancelled}
	task := store.Task{ID: 2, Title: "t", Status: Cancelled, AfterID: 1, Note: "why it stopped"}
	waiting := Waiting
	if _, err := Apply(task, list(dep, task), Edit{Status: &waiting}); err == nil || !strings.Contains(err.Error(), "change what it starts after") {
		t.Fatalf("requeue after a cancelled task = %v", err)
	}
	none := int64(0)
	got, err := Apply(task, list(dep, task), Edit{Status: &waiting, AfterID: &none})
	if err != nil || got.Status != Waiting || got.AfterID != 0 || got.Note != "" {
		t.Fatalf("requeue that clears after = %+v, %v", got, err)
	}
}

func TestEditPlanRetryRequeued(t *testing.T) {
	part := store.Part{ID: 1, Plan: "p", Status: Pending}
	if got, err := EditPlan(store.Task{Status: Planning}, part, "better"); err != nil || got.Plan != "better" {
		t.Fatalf("plan edit = %+v, %v", got, err)
	}
	if _, err := EditPlan(store.Task{Status: Running}, part, "better"); err == nil {
		t.Fatal("a running task's plan was edited")
	}
	failed := store.Part{Status: Failed, Note: "no session"}
	if got, err := Retry(store.Task{Status: NeedsYou}, failed); err != nil || got.Status != Pending || got.Note != "" {
		t.Fatalf("retry = %+v, %v", got, err)
	}
	if _, err := Retry(store.Task{Status: NeedsYou}, store.Part{Status: Running}); err == nil {
		t.Fatal("a running part was retried")
	}
	back := Requeued([]store.Part{{ID: 1, Status: Ready}, {ID: 2, Status: Failed}, {ID: 3, Status: Pending}, {ID: 4, Status: NeedsYou}})
	if len(back) != 2 || back[0].ID != 2 || back[1].ID != 4 || back[0].Status != Pending {
		t.Fatalf("requeued = %+v, want parts 2 and 4 pending; a built part stays built", back)
	}
}

func TestRollup(t *testing.T) {
	ps := func(ss ...string) []store.Part {
		var out []store.Part
		for _, s := range ss {
			out = append(out, store.Part{Status: s})
		}
		return out
	}
	cases := []struct {
		parts []store.Part
		want  string
	}{
		{ps(Running, Ready), Running},
		{ps(Pending, Ready), Running},
		{ps(Ready, Ready), ReadyToMerge},
		{ps(Running, NeedsYou), NeedsYou},
		{ps(Ready, Failed), NeedsYou},
	}
	for _, c := range cases {
		if got := Rollup(c.parts); got != c.want {
			t.Errorf("Rollup(%v) = %s, want %s", c.parts, got, c.want)
		}
	}
}

func TestSlotsCountFeaturesNotClaudes(t *testing.T) {
	tasks := list(store.Task{ID: 1, Status: Running}, store.Task{ID: 2, Status: NeedsYou}, store.Task{ID: 3, Status: NeedsYou})
	parts := []store.Part{
		{TaskID: 1, Status: Running}, {TaskID: 1, Status: Running}, {TaskID: 1, Status: Running},
		{TaskID: 2, Status: Running}, {TaskID: 2, Status: NeedsYou},
		{TaskID: 3, Status: NeedsYou},
	}
	if got := SlotsHeld(tasks, parts); got != 2 {
		t.Fatalf("slots = %d, want 2 (three Claudes of one feature are one slot; a feature only waiting on you holds none)", got)
	}
}

func TestStartable(t *testing.T) {
	tasks := list(
		store.Task{ID: 1, Status: Waiting},
		store.Task{ID: 2, Status: Waiting, AfterID: 1},
		store.Task{ID: 3, Status: Waiting},
		store.Task{ID: 4, Status: Planning},
	)
	parts := []store.Part{{TaskID: 1}, {TaskID: 2}, {TaskID: 4}}
	got := Startable(tasks, parts)
	if len(got) != 1 || got[0].ID != 1 {
		t.Fatalf("startable = %+v, want only #1 (#2 waits on #1, #3 has no parts, #4 is a draft)", got)
	}
}

func TestCounts(t *testing.T) {
	c := Counts(list(store.Task{Status: Waiting}, store.Task{Status: Waiting}, store.Task{Status: Done}))
	if c[Waiting] != 2 || c[Done] != 1 || c[Running] != 0 || len(c) != len(Statuses) {
		t.Fatalf("counts = %v", c)
	}
}

func TestNeighbour_SkipsOtherStatuses(t *testing.T) {
	all := list(store.Task{ID: 1, Status: Waiting}, store.Task{ID: 2, Status: Planning}, store.Task{ID: 3, Status: Waiting})
	if got, ok := Neighbour(all, 3, true); !ok || got.ID != 1 {
		t.Fatalf("up from 3 = %d %v, want 1", got.ID, ok)
	}
	if _, ok := Neighbour(all, 3, false); ok {
		t.Fatal("down from the last waiting task found something")
	}
}
