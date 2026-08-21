package manager

import (
	"context"
	"path/filepath"
	"testing"

	"ccmux.dev/ccmuxd/internal/store"
)

func viewsManager(t *testing.T) *Manager {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "views.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return New(context.Background(), nil, st)
}

// ViewResolver decides which window a workspace's OWNER keeps it in — the
// peers bus and the hub's pane→group index both hang off this. The ladder:
// owner's row wins; the legacy persisted group stands until the import ran;
// after import, a deleted row means put away ("" — bus falls to its directory
// fallback), never a resurrection of the legacy value.
func TestViewResolver_Ladder(t *testing.T) {
	m := viewsManager(t)

	r := m.ViewResolver()
	if g := r("", "w1", "LEGACY"); g != "LEGACY" {
		t.Errorf("no owner → %q, want the legacy group", g)
	}
	if g := r("patric@x.com", "w1", "LEGACY"); g != "LEGACY" {
		t.Errorf("owner without row, unimported → %q, want the legacy group", g)
	}

	if err := m.ImportView("patric@x.com", "w1", "LEGACY"); err != nil {
		t.Fatal(err)
	}
	if err := m.SetView("patric@x.com", "w1", "MOVED"); err != nil {
		t.Fatal(err)
	}
	if g := m.ViewResolver()("patric@x.com", "w1", "LEGACY"); g != "MOVED" {
		t.Errorf("owner's row → %q, want MOVED", g)
	}

	if err := m.SetView("patric@x.com", "w1", ""); err != nil {
		t.Fatal(err)
	}
	if g := m.ViewResolver()("patric@x.com", "w1", "LEGACY"); g != "" {
		t.Errorf("put away after import → %q, want empty (not the legacy group back)", g)
	}
}

// A store-less manager (tests, bare servers) must resolve to legacy, not panic.
func TestViewResolver_NoStore(t *testing.T) {
	m := New(context.Background(), nil, nil)
	if g := m.ViewResolver()("someone@x.com", "w1", "LEGACY"); g != "LEGACY" {
		t.Errorf("store-less resolve = %q, want legacy passthrough", g)
	}
}
