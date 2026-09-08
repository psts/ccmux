package store

import (
	"path/filepath"
	"testing"

	"ccmux.dev/ccmuxd/internal/model"
)

func orderOf(t *testing.T, st *SQLite, wsID string) []string {
	t.Helper()
	all, err := st.Load()
	if err != nil {
		t.Fatal(err)
	}
	for _, w := range all {
		if w.ID == wsID {
			out := make([]string, len(w.Panes))
			for i, p := range w.Panes {
				out[i] = p.ID
			}
			return out
		}
	}
	return nil
}

func equalIDs(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// The position column's three rules, each a stated contract in store.go: a
// SavePane on an existing row never writes position (a stale snapshot must not
// undo a drag), UpdatePanePositions is an UPDATE that skips a closed pane's id
// rather than resurrecting it, and it never reaches into another workspace.
func TestPaneOrder_PositionRules(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "reg.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	for _, ws := range []string{"ws", "other"} {
		if err := st.SaveWorkspace(&model.Workspace{ID: ws, Name: ws}); err != nil {
			t.Fatal(err)
		}
	}
	panes := []*model.Pane{
		{ID: "a", WorkspaceID: "ws", CreatedAt: 1},
		{ID: "b", WorkspaceID: "ws", CreatedAt: 2},
		{ID: "c", WorkspaceID: "ws", CreatedAt: 3},
		{ID: "z", WorkspaceID: "other", CreatedAt: 4, Position: 7},
	}
	for _, p := range panes {
		if err := st.SavePane(p); err != nil {
			t.Fatal(err)
		}
	}

	if err := st.UpdatePanePositions("ws", []string{"c", "a", "b"}); err != nil {
		t.Fatal(err)
	}
	if got := orderOf(t, st, "ws"); !equalIDs(got, []string{"c", "a", "b"}) {
		t.Fatalf("after reorder: %v", got)
	}

	// A stale snapshot of "a" (position 0, as before the drag) rides a title
	// update; the order on disk must not move.
	stale := *panes[0]
	stale.Title = "renamed"
	if err := st.SavePane(&stale); err != nil {
		t.Fatal(err)
	}
	if got := orderOf(t, st, "ws"); !equalIDs(got, []string{"c", "a", "b"}) {
		t.Fatalf("a stale SavePane moved the order: %v", got)
	}

	// A closed pane named in a racing reorder stays gone.
	if err := st.DeletePane("b"); err != nil {
		t.Fatal(err)
	}
	if err := st.UpdatePanePositions("ws", []string{"b", "c", "a"}); err != nil {
		t.Fatal(err)
	}
	if got := orderOf(t, st, "ws"); !equalIDs(got, []string{"c", "a"}) {
		t.Fatalf("a closed pane came back or the rest moved: %v", got)
	}

	// Another workspace's pane is out of reach.
	if err := st.UpdatePanePositions("ws", []string{"z", "a", "c"}); err != nil {
		t.Fatal(err)
	}
	all, err := st.Load()
	if err != nil {
		t.Fatal(err)
	}
	for _, w := range all {
		if w.ID == "other" && (len(w.Panes) != 1 || w.Panes[0].Position != 7) {
			t.Fatalf("other workspace's pane touched: %+v", w.Panes)
		}
	}
}
