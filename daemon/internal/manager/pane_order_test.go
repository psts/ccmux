package manager

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"ccmux.dev/ccmuxd/internal/model"
	"ccmux.dev/ccmuxd/internal/store"
	"ccmux.dev/ccmuxd/internal/tmux"
)

func idsOf(panes []*model.Pane) []string {
	out := make([]string, len(panes))
	for i, p := range panes {
		out[i] = p.ID
	}
	return out
}

func sameIDs(a, b []string) bool {
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

func TestOrderPanes_ListedFirstRestKeepTheirOrder(t *testing.T) {
	panes := []*model.Pane{{ID: "a"}, {ID: "b"}, {ID: "c"}, {ID: "d"}}
	cases := []struct {
		name string
		ids  []string
		want []string
	}{
		{"full permutation", []string{"c", "a", "d", "b"}, []string{"c", "a", "d", "b"}},
		{"partial: rest trail in old order", []string{"d"}, []string{"d", "a", "b", "c"}},
		{"unknown ids ignored, repeats count once", []string{"b", "zzz", "b", "a"}, []string{"b", "a", "c", "d"}},
		{"empty list changes nothing", nil, []string{"a", "b", "c", "d"}},
	}
	for _, c := range cases {
		if got := idsOf(orderPanes(panes, c.ids)); !sameIDs(got, c.want) {
			t.Errorf("%s: got %v want %v", c.name, got, c.want)
		}
	}
}

func TestRenumber(t *testing.T) {
	// Positions can have gaps after a close; renumbering closes them.
	panes := []*model.Pane{{ID: "a", Position: 0}, {ID: "b", Position: 4}}
	if ids := renumber(panes); !sameIDs(ids, []string{"a", "b"}) || panes[1].Position != 1 {
		t.Fatalf("renumber: ids %v, b at %d", ids, panes[1].Position)
	}
}

// A reorder renumbers, persists, and tells the firehose; a restart then serves
// the same order, which is what the web strip and the Mac merge both read.
func TestReorderPanes_PersistsAndAnnounces(t *testing.T) {
	path := filepath.Join(t.TempDir(), "reg.db")
	st, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	m := New(context.Background(), &tmux.Server{Socket: "unused"}, st)
	ws := &model.Workspace{ID: "ws", Name: "w", Panes: []*model.Pane{
		{ID: "a", WorkspaceID: "ws", CreatedAt: 1},
		{ID: "b", WorkspaceID: "ws", CreatedAt: 2},
		{ID: "c", WorkspaceID: "ws", CreatedAt: 3},
	}}
	if err := st.SaveWorkspace(ws); err != nil {
		t.Fatal(err)
	}
	for _, p := range ws.Panes {
		if err := st.SavePane(p); err != nil {
			t.Fatal(err)
		}
	}
	m.byID["ws"] = &entry{ws: ws}
	_, ch := m.events.subscribe()

	got, err := m.ReorderPanes("ws", []string{"c", "a"})
	if err != nil {
		t.Fatal(err)
	}
	if ids := idsOf(got.Panes); !sameIDs(ids, []string{"c", "a", "b"}) {
		t.Fatalf("served order %v", ids)
	}
	for i, p := range got.Panes {
		if p.Position != i {
			t.Errorf("pane %s position %d, want %d", p.ID, p.Position, i)
		}
	}
	select {
	case ev := <-ch:
		if ev.Kind != "workspace-status" || ev.WorkspaceID != "ws" {
			t.Fatalf("event %+v", ev)
		}
	default:
		t.Fatal("no firehose event: other lenses would never redraw")
	}
	// A drag that lists a terminal ahead of an agent still serves, stores and
	// announces the agent first; the terminals keep the dragged order.
	ws.Panes[2].Harness = "claude" // b
	if err := st.SavePane(ws.Panes[2]); err != nil {
		t.Fatal(err)
	}
	got, err = m.ReorderPanes("ws", []string{"a", "c"})
	if err != nil {
		t.Fatal(err)
	}
	if ids := idsOf(got.Panes); !sameIDs(ids, []string{"b", "a", "c"}) {
		t.Fatalf("drag of terminals ahead of an agent served %v, want [b a c]", ids)
	}
	if _, err := m.ReorderPanes("nope", []string{"a"}); err == nil {
		t.Fatal("unknown workspace accepted")
	}

	st.Close()
	st2, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer st2.Close()
	all, err := st2.Load()
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 1 || !sameIDs(idsOf(all[0].Panes), []string{"b", "a", "c"}) {
		t.Fatalf("after restart: %+v", all)
	}
}

func TestRenameWorkspace_PersistsAndAnnounces(t *testing.T) {
	path := filepath.Join(t.TempDir(), "reg.db")
	st, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	m := New(context.Background(), &tmux.Server{Socket: "unused"}, st)
	ws := &model.Workspace{ID: "ws", Name: "🐦 x-poster", Agent: "x-poster"}
	if err := st.SaveWorkspace(ws); err != nil {
		t.Fatal(err)
	}
	m.byID["ws"] = &entry{ws: ws}
	_, ch := m.events.subscribe()

	if err := m.RenameWorkspace("ws", "🚀 x-poster"); err != nil {
		t.Fatal(err)
	}
	if got := m.Workspace("ws").Name; got != "🚀 x-poster" {
		t.Fatalf("served name %q", got)
	}
	select {
	case ev := <-ch:
		if ev.Kind != "workspace-status" || ev.WorkspaceID != "ws" {
			t.Fatalf("event %+v", ev)
		}
	default:
		t.Fatal("no firehose event: lenses would never redraw")
	}
	// Same name again: nothing to say, nothing announced.
	if err := m.RenameWorkspace("ws", "🚀 x-poster"); err != nil {
		t.Fatal(err)
	}
	select {
	case ev := <-ch:
		t.Fatalf("no-op rename announced %+v", ev)
	default:
	}
	if err := m.RenameWorkspace("nope", "x"); !errors.Is(err, ErrUnknownWorkspace) {
		t.Fatalf("unknown workspace = %v, want ErrUnknownWorkspace", err)
	}

	st.Close()
	st2, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer st2.Close()
	all, err := st2.Load()
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 1 || all[0].Name != "🚀 x-poster" {
		t.Fatalf("after restart: %+v", all)
	}
}

// Agent tabs (a pane with a harness: claude, opencode, pi, codex) come before
// plain terminals, each group keeping its own order, so attaching a session
// lands on its agent rather than on whichever terminal was opened first.
func TestAgentsFirst(t *testing.T) {
	panes := []*model.Pane{
		{ID: "term1"}, {ID: "claude", Harness: "claude"}, {ID: "term2"}, {ID: "pi", Harness: "pi"},
	}
	got := idsOf(agentsFirst(panes))
	if want := []string{"claude", "pi", "term1", "term2"}; !sameIDs(got, want) {
		t.Fatalf("agentsFirst = %v, want %v", got, want)
	}
	// A drag that puts a terminal first still serves the agents first, in the
	// order the drag gave them.
	got = idsOf(agentsFirst(orderPanes(panes, []string{"term2", "pi", "claude"})))
	if want := []string{"pi", "claude", "term2", "term1"}; !sameIDs(got, want) {
		t.Fatalf("after a drag = %v, want %v", got, want)
	}
}
