package store

import "testing"

func TestTasks_AddQueuesAtTheBack(t *testing.T) {
	st := taskStore(t)
	a, err := st.AddTask(Task{RepoPath: "/r/a", Title: "one", Plan: "p", Status: "waiting", CreatedAt: 1, UpdatedAt: 1})
	if err != nil {
		t.Fatal(err)
	}
	b, err := st.AddTask(Task{RepoPath: "/r/b", Title: "two", Plan: "p", Status: "waiting", AfterID: a, CreatedAt: 2, UpdatedAt: 2})
	if err != nil {
		t.Fatal(err)
	}
	list, err := st.Tasks()
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 2 || list[0].ID != a || list[1].ID != b {
		t.Fatalf("order = %+v, want %d then %d", list, a, b)
	}
	if list[0].Position >= list[1].Position {
		t.Fatalf("positions %d, %d: the later task must sort after", list[0].Position, list[1].Position)
	}
	if list[1].AfterID != a {
		t.Fatalf("after = %d, want %d", list[1].AfterID, a)
	}
}

func TestTasks_SaveSwapDelete(t *testing.T) {
	st := taskStore(t)
	a, _ := st.AddTask(Task{RepoPath: "/r", Title: "one", Plan: "p", Status: "waiting"})
	b, _ := st.AddTask(Task{RepoPath: "/r", Title: "two", Plan: "p", Status: "waiting"})
	ta, _ := st.Task(a)
	tb, _ := st.Task(b)
	if err := st.SwapTaskPositions(*ta, *tb); err != nil {
		t.Fatal(err)
	}
	list, _ := st.Tasks()
	if list[0].ID != b {
		t.Fatalf("after swap first = %d, want %d", list[0].ID, b)
	}
	ta.Status, ta.Plan, ta.Note = "cancelled", "new plan", "why"
	if ok, err := st.SaveTask(*ta); err != nil || !ok {
		t.Fatalf("save = %v, %v", ok, err)
	}
	got, _ := st.Task(a)
	if got.Status != "cancelled" || got.Plan != "new plan" || got.Note != "why" || got.Position != tb.Position {
		t.Fatalf("saved = %+v (save must not undo the swap's position)", got)
	}
	if ok, _ := st.DeleteTask(a); !ok {
		t.Fatal("delete reported nothing removed")
	}
	if got, err := st.Task(a); err != nil || got != nil {
		t.Fatalf("deleted task = %+v, %v; want nil, nil", got, err)
	}
	if ok, _ := st.SaveTask(Task{ID: a}); ok {
		t.Fatal("save of a deleted task reported success")
	}
}
