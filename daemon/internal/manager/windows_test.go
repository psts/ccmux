package manager

import (
	"context"
	"path/filepath"
	"testing"

	"ccmux.dev/ccmuxd/internal/store"
)

func windowsManager(t *testing.T) *Manager {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "windows.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return New(context.Background(), nil, st)
}

// The group ladder over the shared snapshot: membership wins; the legacy
// persisted group stands until the import ran; after import, no membership
// means "" (the bus falls to its directory fallback), never the legacy back.
func TestSharedGroupResolver_Ladder(t *testing.T) {
	m := windowsManager(t)

	if g := m.SharedGroupResolver()("w1", "LEGACY"); g != "LEGACY" {
		t.Errorf("unimported, no membership → %q, want the legacy group", g)
	}
	if err := m.SeedWindowMembership("w1", "CHARTLABS"); err != nil {
		t.Fatal(err)
	}
	if g := m.SharedGroupResolver()("w1", "LEGACY"); g != "CHARTLABS" {
		t.Errorf("membership → %q, want CHARTLABS", g)
	}
	if err := m.AssignWorkspace("w1", ""); err != nil {
		t.Fatal(err)
	}
	if g := m.SharedGroupResolver()("w1", "LEGACY"); g != "" {
		t.Errorf("removed after import → %q, want empty (not the legacy group back)", g)
	}
}

// A store-less manager resolves to legacy, not a panic.
func TestSharedGroupResolver_NoStore(t *testing.T) {
	m := New(context.Background(), nil, nil)
	if g := m.SharedGroupResolver()("w1", "LEGACY"); g != "LEGACY" {
		t.Errorf("store-less resolve = %q, want legacy passthrough", g)
	}
}

// The v1→v2 migration: per-login rows become shared windows (names merged
// case-insensitively), one membership per workspace, open flags per login —
// and it runs exactly once.
func TestMigrateViewsToWindows(t *testing.T) {
	m := windowsManager(t)
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(m.store.SetView("patric@x.com", "w1", "CHARTLABS"))
	must(m.store.SetView("dasha@x.com", "w1", "chartlabs")) // same window, other spelling
	must(m.store.SetView("dasha@x.com", "w2", "dasha"))
	must(m.MigrateViewsToWindows())

	windows := m.Windows()
	if len(windows) != 2 {
		t.Fatalf("%d windows after migration, want CHARTLABS + dasha merged case-insensitively: %+v", len(windows), windows)
	}
	if g := m.SharedGroupResolver()("w1", ""); g != "CHARTLABS" && g != "chartlabs" {
		t.Fatalf("w1 membership = %q", g)
	}
	if g := m.SharedGroupResolver()("w2", ""); g != "dasha" {
		t.Fatalf("w2 membership = %q", g)
	}
	wid, _ := m.WindowByName("chartlabs")
	for _, w := range windows {
		if w.ID == wid && len(w.OpenBy) != 2 {
			t.Fatalf("both logins should hold CHARTLABS open after migration: %+v", w)
		}
	}

	// Idempotent: a second run must not resurrect anything later removed.
	must(m.AssignWorkspace("w2", ""))
	must(m.MigrateViewsToWindows())
	if g := m.SharedGroupResolver()("w2", ""); g != "" {
		t.Fatalf("second migration resurrected w2 into %q", g)
	}
}

// An empty views table (the recommended wipe) migrates to nothing.
func TestMigrateViewsToWindows_EmptyIsClean(t *testing.T) {
	m := windowsManager(t)
	if err := m.MigrateViewsToWindows(); err != nil {
		t.Fatal(err)
	}
	if n := len(m.Windows()); n != 0 {
		t.Fatalf("%d windows from an empty migration", n)
	}
}
