package model

import "testing"

// The sidebar renders workspace lists verbatim, so this order IS the UI order:
// alphabetical, case-insensitive, deterministic for equal names (ID tiebreak).
func TestSortByName(t *testing.T) {
	wss := []*Workspace{
		{ID: "3", Name: "voc"},
		{ID: "2", Name: "Polytrader"},
		{ID: "1", Name: "gamla-blocket"},
		{ID: "b", Name: "voc"},
	}
	SortByName(wss)
	got := ""
	for _, ws := range wss {
		got += ws.Name + "/" + ws.ID + " "
	}
	want := "gamla-blocket/1 Polytrader/2 voc/3 voc/b "
	if got != want {
		t.Fatalf("order = %q, want %q", got, want)
	}
}
