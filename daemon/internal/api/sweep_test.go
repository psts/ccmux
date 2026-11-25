package api

import (
	"encoding/json"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"ccmux.dev/ccmuxd/internal/store"
)

// The disks' line is in GET /v1/health as its supplier gives it, and left
// out where there is none.
func TestHealth_Disks(t *testing.T) {
	s := &Server{}
	read := func() map[string]any {
		rec := httptest.NewRecorder()
		s.health(rec, httptest.NewRequest("GET", "/v1/health", nil))
		var got map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
			t.Fatal(err)
		}
		return got
	}
	if _, ok := read()["disks"]; ok {
		t.Fatal("no supplier, no field")
	}
	s.SetDiskHealth(func() any {
		return []map[string]any{{"mount": "/ext", "freeBytes": 1, "totalBytes": 10, "low": true}}
	})
	disks, ok := read()["disks"].([]any)
	if !ok || len(disks) != 1 || disks[0].(map[string]any)["mount"] != "/ext" {
		t.Fatalf("disks = %v", read()["disks"])
	}
}

// A to-do filed once is one item however its numbers change.
func TestAddTodoOnceIsOneItem(t *testing.T) {
	s := &Server{}
	if _, err := s.AddTodoOnce("/r", "a (", "a (1)", "low", "x"); err == nil {
		t.Fatal("no lists, no item, and an error that says so")
	}
	st, err := store.Open(filepath.Join(t.TempDir(), "r.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	s.SetTodos(st)
	repo := t.TempDir()
	if added, err := s.AddTodoOnce(repo, "worktree x (", "worktree x (1 GB)", "low", "weekly sweep"); err != nil || !added {
		t.Fatalf("first: %v %v", added, err)
	}
	if added, err := s.AddTodoOnce(repo, "worktree x (", "worktree x (2 GB)", "low", "weekly sweep"); err != nil || added {
		t.Fatalf("again: %v %v", added, err)
	}
}
