package store

import "testing"

// addTask files a task with one part in /r/app, the only way tasks are made.
func addTask(st *SQLite, t Task) (int64, error) {
	return st.AddTaskWithPart(t, Part{RepoPath: "/r/app", Plan: "p", Status: "pending"})
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
	id, err := st.AddTaskWithPart(Task{Title: "feature", Status: "planning"},
		Part{RepoPath: "/r/app", Plan: "app plan", Status: "pending", FiledBy: "app"})
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
