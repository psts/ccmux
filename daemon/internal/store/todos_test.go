package store

import (
	"strings"
	"testing"
)

func addTodo(t *testing.T, st *SQLite, repo, text, norm string) int64 {
	t.Helper()
	id, err := st.AddTodo(Todo{Repo: repo, Text: text, Urgency: "low", Status: "open", AddedBy: "app", CreatedAt: 10, UpdatedAt: 10}, norm)
	if err != nil {
		t.Fatalf("add %q: %v", text, err)
	}
	return id
}

func TestTodos_AnItemIsKeptAndListedByRepo(t *testing.T) {
	st := openTestStore(t)
	a := addTodo(t, st, "/r/app", "Fix the flaky test", "fix the flaky test")
	addTodo(t, st, "/r/blog", "Update the readme", "update the readme")

	got, err := st.Todo(a)
	if err != nil || got == nil {
		t.Fatalf("todo %d: %v, %v", a, got, err)
	}
	want := Todo{ID: a, Repo: "/r/app", Text: "Fix the flaky test", Urgency: "low", Status: "open", AddedBy: "app", CreatedAt: 10, UpdatedAt: 10}
	if *got != want {
		t.Fatalf("todo = %+v, want %+v", *got, want)
	}
	if none, err := st.Todo(999); err != nil || none != nil {
		t.Fatalf("a number nobody has = %v, %v; want nil, nil", none, err)
	}
	for repo, n := range map[string]int{"/r/app": 1, "/r/blog": 1, "": 2, "/r/none": 0} {
		list, err := st.Todos(repo)
		if err != nil || len(list) != n {
			t.Fatalf("Todos(%q) = %d items, %v; want %d", repo, len(list), err, n)
		}
	}
}

func TestTodos_ARepoNeverHoldsTheSameUnfinishedItemTwice(t *testing.T) {
	st := openTestStore(t)
	first := addTodo(t, st, "/r/app", "Fix the flaky test", "fix the flaky test")

	if _, err := st.AddTodo(Todo{Repo: "/r/app", Text: "fix the flaky test.", Urgency: "high", Status: "open"}, "fix the flaky test"); err == nil ||
		!strings.Contains(err.Error(), "UNIQUE") {
		t.Fatalf("a second unfinished copy was added (err %v); the index should refuse it", err)
	}
	// The same words in another repo are another item.
	addTodo(t, st, "/r/blog", "Fix the flaky test", "fix the flaky test")

	found, err := st.UnfinishedTodo("/r/app", "fix the flaky test")
	if err != nil || found == nil || found.ID != first {
		t.Fatalf("UnfinishedTodo = %+v, %v; want #%d", found, err, first)
	}
	// Once it is done, the same thing seen again is a new item.
	done := *found
	done.Status, done.DoneBy, done.DoneAt = "done", "app", 20
	if ok, err := st.SaveTodo(done, "fix the flaky test"); err != nil || !ok {
		t.Fatalf("save done: %v, %v", ok, err)
	}
	if left, err := st.UnfinishedTodo("/r/app", "fix the flaky test"); err != nil || left != nil {
		t.Fatalf("a done item still reads as unfinished: %+v, %v", left, err)
	}
	addTodo(t, st, "/r/app", "Fix the flaky test", "fix the flaky test")
}

func TestTodos_SaveWritesWhatChangesAndDeleteRemoves(t *testing.T) {
	st := openTestStore(t)
	id := addTodo(t, st, "/r/app", "Fix the flaky test", "fix the flaky test")

	got, _ := st.Todo(id)
	next := *got
	next.Text, next.Urgency, next.Status, next.TaskID, next.Note, next.UpdatedAt = "Fix it", "high", "tasked", 7, "sent", 30
	next.Repo, next.AddedBy, next.CreatedAt = "/elsewhere", "someone", 999 // never written
	if ok, err := st.SaveTodo(next, "fix it"); err != nil || !ok {
		t.Fatalf("save: %v, %v", ok, err)
	}
	got, _ = st.Todo(id)
	want := Todo{ID: id, Repo: "/r/app", Text: "Fix it", Urgency: "high", Status: "tasked", TaskID: 7, AddedBy: "app", Note: "sent", CreatedAt: 10, UpdatedAt: 30}
	if *got != want {
		t.Fatalf("after save = %+v, want %+v", *got, want)
	}
	withTask, err := st.TodosWithTask()
	if err != nil || len(withTask) != 1 || withTask[0].ID != id {
		t.Fatalf("TodosWithTask = %+v, %v; want only #%d", withTask, err, id)
	}

	if ok, err := st.DeleteTodo(id); err != nil || !ok {
		t.Fatalf("delete: %v, %v", ok, err)
	}
	if ok, _ := st.DeleteTodo(id); ok {
		t.Fatal("deleting it twice said it was there both times")
	}
	if ok, _ := st.SaveTodo(next, "fix it"); ok {
		t.Fatal("saving a deleted item said it was written")
	}
}
