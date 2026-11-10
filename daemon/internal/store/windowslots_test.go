package store

import "testing"

// A window's task slots are one row per window: the last write wins, 0 is a
// value (the queue is held) and not "unset", it survives a reopen, a write
// for a window that does not exist lands nowhere, and deleting the window
// takes its number with it.
func TestWindowSlotsRoundTrip(t *testing.T) {
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
	set := func(id string, n int) bool {
		t.Helper()
		ok, err := s.SetWindowSlots(id, n)
		must(err)
		return ok
	}
	if !set("win-a", 3) || !set("win-a", 2) || !set("win-b", 0) {
		t.Fatal("slots for an existing window must land")
	}
	if set("win-gone", 1) {
		t.Fatal("slots for an unknown window must land nowhere")
	}
	must(s.Close())

	s, err = Open(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer s.Close()
	got, err := s.WindowSlots()
	must(err)
	if n, ok := got["win-b"]; len(got) != 2 || got["win-a"] != 2 || !ok || n != 0 {
		t.Fatalf("slots = %v, want win-a 2 and win-b 0", got)
	}

	must(s.DeleteWindow("win-a"))
	got, err = s.WindowSlots()
	must(err)
	if _, ok := got["win-a"]; ok || len(got) != 1 {
		t.Fatalf("slots after delete = %v, want only win-b", got)
	}
}
