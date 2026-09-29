package store

import "testing"

// A shared window's size is one row per window: the last write wins, it
// survives a reopen, a write for a window that does not exist lands nowhere,
// and deleting the window takes its size with it.
func TestWindowSizeRoundTrip(t *testing.T) {
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
	set := func(id string, size WindowSize) bool {
		t.Helper()
		ok, err := s.SetWindowSize(id, size)
		must(err)
		return ok
	}
	if !set("win-a", WindowSize{Width: 1200, Height: 800}) || !set("win-a", WindowSize{Width: 1600, Height: 1000}) {
		t.Fatal("a size for an existing window must land")
	}
	if set("win-gone", WindowSize{Width: 900, Height: 700}) {
		t.Fatal("a size for an unknown window must land nowhere")
	}
	must(s.Close())

	s, err = Open(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer s.Close()
	sizes, err := s.WindowSizes()
	must(err)
	if len(sizes) != 1 || sizes["win-a"] != (WindowSize{Width: 1600, Height: 1000}) {
		t.Fatalf("sizes = %v, want only win-a at the last write", sizes)
	}

	must(s.DeleteWindow("win-a"))
	sizes, err = s.WindowSizes()
	must(err)
	if len(sizes) != 0 {
		t.Fatalf("sizes after delete = %v, want none", sizes)
	}
}
