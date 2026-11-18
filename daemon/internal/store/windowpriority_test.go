package store

import (
	"database/sql"
	"testing"
)

// A window's task priority is one row per window: the last write wins, 0
// clears it (and clearing one that has none is still a known window), it
// survives a reopen, a write for a window that does not exist lands
// nowhere, and deleting the window takes its priority with it.
func TestWindowPriorityRoundTrip(t *testing.T) {
	path := t.TempDir() + "/reg.db"
	s, err := Open(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(s.CreateWindow("win-a", "ALPHA"))
	must(s.CreateWindow("win-b", "BETA"))
	must(s.CreateWindow("win-c", "GAMMA"))
	set := func(id string, n int) bool {
		t.Helper()
		ok, err := s.SetWindowPriority(id, n)
		must(err)
		return ok
	}
	if !set("win-a", 3) || !set("win-a", 1) || !set("win-b", 2) || !set("win-c", 5) || !set("win-c", 0) || !set("win-c", 0) {
		t.Fatal("a priority for an existing window must land")
	}
	if set("win-gone", 1) || set("win-gone", 0) {
		t.Fatal("a priority for an unknown window must land nowhere")
	}
	must(s.Close())

	s, err = Open(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer s.Close()
	got, err := s.WindowPriorities()
	must(err)
	if len(got) != 2 || got["win-a"] != 1 || got["win-b"] != 2 {
		t.Fatalf("priorities = %v, want win-a 1 and win-b 2", got)
	}
	must(s.DeleteWindow("win-a"))
	got, err = s.WindowPriorities()
	must(err)
	if _, ok := got["win-a"]; ok || len(got) != 1 {
		t.Fatalf("priorities after delete = %v, want only win-b", got)
	}
}

// A database made before priorities existed gains the table on start, and
// its windows and their slots are untouched.
func TestWindowPriorityOnAnOldDatabase(t *testing.T) {
	path := t.TempDir() + "/old.db"
	s, err := Open(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if err := s.CreateWindow("win-a", "ALPHA"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SetWindowSlots("win-a", 2); err != nil {
		t.Fatal(err)
	}
	s.Close()
	// The old schema: everything but the priority table.
	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := raw.Exec(`DROP TABLE window_priority`); err != nil {
		t.Fatalf("drop: %v", err)
	}
	raw.Close()

	s, err = Open(path)
	if err != nil {
		t.Fatalf("open the old database: %v", err)
	}
	defer s.Close()
	if got, err := s.WindowPriorities(); err != nil || len(got) != 0 {
		t.Fatalf("priorities on an old database = %v, %v; want none and no error", got, err)
	}
	if ok, err := s.SetWindowPriority("win-a", 1); err != nil || !ok {
		t.Fatalf("set on an old database = %v, %v", ok, err)
	}
	if slots, _ := s.WindowSlots(); slots["win-a"] != 2 {
		t.Fatalf("slots = %v, want win-a 2 kept", slots)
	}
}
