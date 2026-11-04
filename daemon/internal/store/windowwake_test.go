package store

import (
	"reflect"
	"testing"
)

// A session marked "wake when its window opens" keeps the mark across a
// restart and until it is cleared. Setting a window's marks replaces the
// marks of exactly that window's members, and never marks a non-member.
// Reading answers in the order asked and as an empty list, never nil, when
// nothing is marked.
func TestWakeMarks(t *testing.T) {
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
	marks := func(ids ...string) []string {
		t.Helper()
		got, err := s.WakeMarks(ids)
		must(err)
		return got
	}
	chartlabs := []string{"ws-1", "ws-2", "ws-3"}
	mixed := []string{"ws-8", "ws-9"}

	if got := marks(chartlabs...); got == nil || len(got) != 0 {
		t.Fatalf("nothing marked = %#v; want an empty list", got)
	}
	must(s.SetWakeMarks(mixed, []string{"ws-9"}))
	must(s.SetWakeMarks(chartlabs, []string{"ws-3", "ws-1", "ws-x"}))
	must(s.Close())

	s, err = Open(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer s.Close()
	if got := marks(chartlabs...); !reflect.DeepEqual(got, []string{"ws-1", "ws-3"}) {
		t.Fatalf("after a restart = %v; want [ws-1 ws-3] (ws-x is no member)", got)
	}
	if got := marks(chartlabs...); !reflect.DeepEqual(got, []string{"ws-1", "ws-3"}) {
		t.Fatalf("a second read = %v; reading must not use the marks up", got)
	}

	must(s.SetWakeMarks(chartlabs, []string{"ws-2"})) // a later close replaces
	if got := marks(chartlabs...); !reflect.DeepEqual(got, []string{"ws-2"}) {
		t.Fatalf("after a later close = %v; want [ws-2]", got)
	}
	if got := marks(mixed...); !reflect.DeepEqual(got, []string{"ws-9"}) {
		t.Fatalf("another window's marks = %v; want [ws-9] untouched", got)
	}

	must(s.ClearWakeMark("ws-2"))
	if got := marks(chartlabs...); len(got) != 0 {
		t.Fatalf("after clearing ws-2 = %v; want none", got)
	}
}
