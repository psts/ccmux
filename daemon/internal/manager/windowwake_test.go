package manager

import (
	"reflect"
	"testing"
)

// A window's wake marks are kept per member: marking one window leaves
// another window's marks alone, a session outside the window is never
// marked, reading does not use the marks up, and a cleared mark is gone.
func TestWindowWakeMarks(t *testing.T) {
	m := windowsManager(t)
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(m.store.CreateWindow("cl", "CHARTLABS"))
	must(m.store.CreateWindow("mx", "MIXED"))
	for ws, win := range map[string]string{"ws-1": "cl", "ws-2": "cl", "ws-8": "mx"} {
		must(m.store.SetWindowMember(ws, win))
	}
	wake := func(win string) []string {
		t.Helper()
		got, err := m.WindowWake(win)
		must(err)
		return got
	}

	must(m.MarkWindowWake("mx", []string{"ws-8"}))
	must(m.MarkWindowWake("cl", []string{"ws-2", "ws-8"}))
	if got := wake("cl"); !reflect.DeepEqual(got, []string{"ws-2"}) {
		t.Fatalf("chartlabs = %v; want [ws-2] (ws-8 belongs to another window)", got)
	}
	if got := wake("cl"); !reflect.DeepEqual(got, []string{"ws-2"}) {
		t.Fatalf("a second read = %v; reading must not use the marks up", got)
	}
	if got := wake("mx"); !reflect.DeepEqual(got, []string{"ws-8"}) {
		t.Fatalf("mixed = %v; want [ws-8] untouched by chartlabs's marks", got)
	}

	must(m.ClearWakeMark("ws-2"))
	if got := wake("cl"); got == nil || len(got) != 0 {
		t.Fatalf("after waking ws-2 = %#v; want an empty list", got)
	}
}
