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
	tasks := list(store.Task{ID: 1, Status: Running, WindowID: "w1"}, store.Task{ID: 2, Status: NeedsYou, WindowID: "w1"},
		store.Task{ID: 3, Status: NeedsYou, WindowID: "w1"}, store.Task{ID: 4, Status: Running, WindowID: "w2"})
	parts := []store.Part{
		{TaskID: 1, Status: Running}, {TaskID: 1, Status: Running}, {TaskID: 1, Status: Running},
		{TaskID: 2, Status: Running}, {TaskID: 2, Status: NeedsYou},
		{TaskID: 3, Status: NeedsYou},
		{TaskID: 4, Status: Running},
	}
	got := SlotsHeld(tasks, parts)
	if got["w1"] != 2 || got["w2"] != 1 || len(got) != 2 {
		t.Fatalf("slots = %v, want w1:2 w2:1 (three Claudes of one feature are one slot; a feature only waiting on you holds none; each window counts its own)", got)
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

// Ahead counts the unfinished tasks a task waits on through its "starts
// after" chain, and -1 when the chain reaches a cancelled or removed task
// or loops: it then holds until a person acts.
func TestAhead(t *testing.T) {
	all := []store.Task{
		{ID: 7, Status: Done},
		{ID: 9, Status: Cancelled},
		{ID: 10, Status: Running},
		{ID: 11, Status: Waiting, AfterID: 10},
		{ID: 12, Status: Waiting, AfterID: 11},
		{ID: 14, Status: Waiting, AfterID: 9},
		{ID: 15, Status: Waiting},
		{ID: 16, Status: Waiting, AfterID: 7},
		{ID: 17, Status: Waiting, AfterID: 99},
		{ID: 18, Status: Waiting, AfterID: 19},
		{ID: 19, Status: Waiting, AfterID: 18},
	}
	want := map[int64]int{11: 1, 12: 2, 14: -1, 15: 0, 16: 0, 17: -1, 18: -1, 19: -1}
	for id, n := range want {
		if got := Ahead(all, id); got != n {
			t.Errorf("Ahead(%d) = %d, want %d", id, got, n)
		}
	}
}

// A waiting task trades places only with the nearest waiting task as far
// from starting as it is: the one next to it in a list sorted by Ahead, so
// the move shows. Planning tasks keep plain queue order.
func TestNeighbour_WaitingStaysInItsPlaceInLine(t *testing.T) {
	all := []store.Task{
		{ID: 1, Status: Running},
		{ID: 2, Status: Waiting, AfterID: 1}, // one ahead
		{ID: 3, Status: Waiting},             // can start
		{ID: 4, Status: Planning},
		{ID: 5, Status: Waiting},             // can start
		{ID: 6, Status: Waiting, AfterID: 1}, // one ahead
		{ID: 8, Status: Planning, AfterID: 1},
	}
	cases := []struct {
		id   int64
		up   bool
		want int64 // 0: no move
	}{
		{5, true, 3}, {3, false, 5}, {3, true, 0}, {5, false, 0},
		{6, true, 2}, {2, false, 6}, {2, true, 0},
		{8, true, 4}, {4, false, 8},
	}
	for _, c := range cases {
		got, ok := Neighbour(all, c.id, c.up)
		if c.want == 0 && ok {
			t.Errorf("Neighbour(%d, up=%v) = #%d, want none", c.id, c.up, got.ID)
		}
		if c.want != 0 && (!ok || got.ID != c.want) {
			t.Errorf("Neighbour(%d, up=%v) = #%d %v, want #%d", c.id, c.up, got.ID, ok, c.want)
		}
	}
}

// Retry on a feature with nothing at work puts it back in the queue as
// Waiting with every unbuilt part to start again, so it waits its turn like
// any task; a built part stays built. With another part at work, only the
// retried part goes back to start and the feature keeps its status.
func TestRetryTask(t *testing.T) {
	failed := store.Part{ID: 2, TaskID: 1, Status: Failed, Note: "its session closed"}
	built := store.Part{ID: 1, TaskID: 1, Status: Ready}
	other := store.Part{ID: 3, TaskID: 1, Status: Failed}
	task, parts, err := RetryTask(store.Task{ID: 1, Status: NeedsYou}, []store.Part{built, failed, other}, failed)
	if err != nil || task.Status != Waiting {
		t.Fatalf("idle feature: status %s err %v, want waiting", task.Status, err)
	}
	if len(parts) != 2 || parts[0].ID != 2 || parts[1].ID != 3 || parts[0].Status != Pending || parts[0].Note != "" || parts[1].Status != Pending {
		t.Fatalf("idle feature parts = %+v, want #2 and #3 pending, #1 untouched", parts)
	}

	busy := store.Part{ID: 4, TaskID: 1, Status: Running}
	task, parts, err = RetryTask(store.Task{ID: 1, Status: NeedsYou}, []store.Part{busy, failed, other}, failed)
	if err != nil || task.Status != NeedsYou || len(parts) != 1 || parts[0].ID != 2 || parts[0].Status != Pending {
		t.Fatalf("busy feature: %s %+v %v, want needs_you with only #2 pending", task.Status, parts, err)
	}

	asking := store.Part{ID: 5, TaskID: 1, Status: NeedsYou}
	task, parts, _ = RetryTask(store.Task{ID: 1, Status: NeedsYou}, []store.Part{asking, failed}, failed)
	if task.Status != NeedsYou || len(parts) != 1 || parts[0].ID != 2 {
		t.Fatalf("feature with a part asking: %s %+v, want needs_you and only #2 back to start", task.Status, parts)
	}

	if _, _, err := RetryTask(store.Task{ID: 1, Status: NeedsYou}, []store.Part{built}, built); err == nil {
		t.Fatal("a built part was retried")
	}
}
