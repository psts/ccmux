package store

import "testing"

// addTask files a task with one part in /r/app, the only way tasks are made.
func addTask(st *SQLite, t Task) (int64, error) {
	return st.AddTaskWithParts(t, []Part{{RepoPath: "/r/app", Plan: "p", Status: "pending"}})
}

func TestTasks_AddQueuesAtTheBack(t *testing.T) {
	st := taskStore(t)
	a, err := addTask(st, Task{Title: "one", Status: "waiting", CreatedAt: 1, UpdatedAt: 1})
	if err != nil {
		t.Fatal(err)
	}
	b, err := addTask(st, Task{Title: "two", Status: "waiting", AfterID: a, CreatedAt: 2, UpdatedAt: 2})
	if err != nil {
		t.Fatal(err)
	}
	list, err := st.Tasks()
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 2 || list[0].ID != a || list[1].ID != b || list[0].Position >= list[1].Position {
		t.Fatalf("order = %+v", list)
	}
	if list[1].AfterID != a {
		t.Fatalf("after = %d, want %d", list[1].AfterID, a)
	}
}

func TestTasks_SaveSwapDelete(t *testing.T) {
	st := taskStore(t)
	a, _ := addTask(st, Task{Title: "one", Status: "waiting"})
	b, _ := addTask(st, Task{Title: "two", Status: "waiting"})
	ta, _ := st.Task(a)
	tb, _ := st.Task(b)
	if err := st.SwapTaskPositions(*ta, *tb); err != nil {
		t.Fatal(err)
	}
	if list, _ := st.Tasks(); list[0].ID != b {
		t.Fatalf("after swap first = %d, want %d", list[0].ID, b)
	}
	ta.Status, ta.Title, ta.Note = "cancelled", "renamed", "why"
	if ok, err := st.SaveTask(*ta); err != nil || !ok {
		t.Fatalf("save = %v, %v", ok, err)
	}
	got, _ := st.Task(a)
	if got.Status != "cancelled" || got.Title != "renamed" || got.Note != "why" || got.Position != tb.Position {
		t.Fatalf("saved = %+v (save must not undo the swap's position)", got)
	}
	if ok, _ := st.DeleteTask(a); !ok {
		t.Fatal("delete reported nothing removed")
	}
	if got, err := st.Task(a); err != nil || got != nil {
		t.Fatalf("deleted task = %+v, %v; want nil, nil", got, err)
	}
	if parts, _ := st.Parts(); len(parts) != 1 || parts[0].TaskID != b {
		t.Fatalf("parts after delete = %+v; want only the other task's", parts)
	}
}

func TestParts_OnePerRepoAndSave(t *testing.T) {
	st := taskStore(t)
	id, err := st.AddTaskWithParts(Task{Title: "feature", Status: "planning"},
		[]Part{{RepoPath: "/r/app", Plan: "app plan", Status: "pending", FiledBy: "app"}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.AddPart(Part{TaskID: id, RepoPath: "/r/backend", Plan: "be plan", Status: "pending"}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.AddPart(Part{TaskID: id, RepoPath: "/r/app", Plan: "again", Status: "pending"}); err == nil {
		t.Fatal("a second part for the same repo was accepted")
	}
	parts, _ := st.Parts()
	p := parts[0]
	if p.TaskID != id || p.FiledBy != "app" || p.Plan != "app plan" {
		t.Fatalf("first part = %+v", p)
	}
	p.Status, p.Branch, p.Worktree, p.PaneID, p.Plan = "running", "task/1-x", "/wt", "pane-1", "better"
	if ok, err := st.SavePart(p); err != nil || !ok {
		t.Fatalf("save part = %v, %v", ok, err)
	}
	parts, _ = st.Parts()
	if got := parts[0]; got.Status != "running" || got.Branch != "task/1-x" || got.PaneID != "pane-1" || got.Plan != "better" {
		t.Fatalf("saved part = %+v", got)
	}
}

// A task keeps the window it was filed in, and a save never moves it: only
// filing sets the window.
func TestTasks_KeepTheirWindow(t *testing.T) {
	st := taskStore(t)
	id, err := addTask(st, Task{Title: "one", Status: "waiting", WindowID: "win-a"})
	if err != nil {
		t.Fatal(err)
	}
	got, _ := st.Task(id)
	if got == nil || got.WindowID != "win-a" {
		t.Fatalf("filed = %+v, want window win-a", got)
	}
	got.WindowID, got.Title = "win-b", "renamed"
	if _, err := st.SaveTask(*got); err != nil {
		t.Fatal(err)
	}
	if again, _ := st.Task(id); again.WindowID != "win-a" || again.Title != "renamed" {
		t.Fatalf("after a save = %+v, want renamed and still in win-a", again)
	}
}

// A feature is filed whole or not at all: a part that cannot be written
// (two for one repo) leaves no task behind, and no parts is refused.
func TestTasks_FiledWholeOrNotAtAll(t *testing.T) {
	st := taskStore(t)
	if _, err := st.AddTaskWithParts(Task{Title: "none", Status: "waiting"}, nil); err == nil {
		t.Fatal("a task with no parts was filed")
	}
	_, err := st.AddTaskWithParts(Task{Title: "twice", Status: "waiting"}, []Part{
		{RepoPath: "/r/app", Plan: "a", Status: "pending"}, {RepoPath: "/r/app", Plan: "b", Status: "pending"}})
	if err == nil {
		t.Fatal("two parts for one repo were filed")
	}
	if list, _ := st.Tasks(); len(list) != 0 {
		t.Fatalf("a refused filing left %+v", list)
	}
	id, err := st.AddTaskWithParts(Task{Title: "two", Status: "waiting"}, []Part{
		{RepoPath: "/r/app", Plan: "a", Status: "pending"}, {RepoPath: "/r/api", Plan: "b", Status: "pending"}})
	if parts, _ := st.Parts(); err != nil || len(parts) != 2 || parts[0].TaskID != id || parts[1].TaskID != id {
		t.Fatalf("two-part filing = %v, %+v", err, parts)
	}
}

// AutoFixes is the only cap on automatic send-backs after a failed merge:
// it must survive a save and read back, start at none, and not leak to a
// task that was not saved with it.
func TestTasks_AutoFixesRoundTrip(t *testing.T) {
	st := taskStore(t)
	a, _ := addTask(st, Task{Title: "one", Status: "running"})
	b, _ := addTask(st, Task{Title: "two", Status: "running"})
	ta, _ := st.Task(a)
	if ta.AutoFixes != 0 {
		t.Fatalf("a new task starts with %d auto fixes", ta.AutoFixes)
	}
	ta.AutoFixes, ta.Status = 1, "running"
	if ok, err := st.SaveTask(*ta); err != nil || !ok {
		t.Fatalf("save = %v, %v", ok, err)
	}
	got, _ := st.Task(a)
	other, _ := st.Task(b)
	if got.AutoFixes != 1 || got.Status != "running" || other.AutoFixes != 0 {
		t.Fatalf("after save: %+v, other task %+v", got, other)
	}
	if list, _ := st.Tasks(); list[0].AutoFixes != 1 || list[1].AutoFixes != 0 {
		t.Fatalf("list reads auto fixes %d, %d", list[0].AutoFixes, list[1].AutoFixes)
	}
}

// A task and its part are saved together or not at all: with the part's row
// gone, the task's change is rolled back too; with both rows there, both
// land; another task is never touched.
func TestTasks_SaveTaskAndPartIsAllOrNothing(t *testing.T) {
	st := taskStore(t)
	a, _ := addTask(st, Task{Title: "one", Status: "merge_failed"})
	b, _ := addTask(st, Task{Title: "two", Status: "merge_failed"})
	task, _ := st.Task(a)
	var part Part
	parts, _ := st.Parts()
	for _, p := range parts {
		if p.TaskID == a {
			part = p
		}
	}
	task.Status, part.Status = "running", "running"
	missing := part
	missing.ID = 999
	if ok, err := st.SaveTaskAndPart(*task, missing); ok || err != nil {
		t.Fatalf("save with a missing part = %v, %v; want false, nil", ok, err)
	}
	if got, _ := st.Task(a); got.Status != "merge_failed" {
		t.Fatalf("task saved without its part: %s", got.Status)
	}
	if ok, err := st.SaveTaskAndPart(*task, part); !ok || err != nil {
		t.Fatalf("save = %v, %v", ok, err)
	}
	got, _ := st.Task(a)
	other, _ := st.Task(b)
	parts, _ = st.Parts()
	saved := ""
	for _, p := range parts {
		if p.ID == part.ID {
			saved = p.Status
		}
	}
	if got.Status != "running" || saved != "running" || other.Status != "merge_failed" {
		t.Fatalf("after save: task %s, part %s, other task %s", got.Status, saved, other.Status)
	}
}
