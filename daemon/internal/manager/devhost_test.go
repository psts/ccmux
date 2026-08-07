package manager

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"ccmux.dev/ccmuxd/internal/model"
	"ccmux.dev/ccmuxd/internal/store"
	"ccmux.dev/ccmuxd/internal/tmux"
)

// devhostManager builds a store-backed manager with two cold workspaces,
// injected via the package-internal adopt path so no tmux server is touched.
func devhostManager(t *testing.T) (*Manager, *store.SQLite) {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "reg.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	m := New(context.Background(), &tmux.Server{Socket: "unused"}, st)
	for _, ws := range []*model.Workspace{
		{ID: "w1", Name: "chartlabs", RepoPath: "/r1"},
		{ID: "w2", Name: "gnubok", RepoPath: "/r2"},
	} {
		if err := st.SaveWorkspace(ws); err != nil {
			t.Fatal(err)
		}
		m.adopt(ws, false)
	}
	return m, st
}

func TestSetHostnames_RoundtripAndUniqueness(t *testing.T) {
	m, st := devhostManager(t)

	// Happy path: names normalize to lowercase; persisted through the store.
	ws, err := m.SetHostnames("w1", []model.Hostname{{Name: "ChartLabs-App", Port: 3001}})
	if err != nil {
		t.Fatalf("set: %v", err)
	}
	if len(ws.Hostnames) != 1 || ws.Hostnames[0].Name != "chartlabs-app" {
		t.Fatalf("hostnames = %+v", ws.Hostnames)
	}
	loaded, err := st.Load()
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	for _, l := range loaded {
		if l.ID == "w1" && (len(l.Hostnames) != 1 || l.Hostnames[0].Port != 3001) {
			t.Fatalf("persisted = %+v", l.Hostnames)
		}
	}

	// Cross-workspace uniqueness, then release-and-reclaim.
	if _, err := m.SetHostnames("w2", []model.Hostname{{Name: "chartlabs-app", Port: 4000}}); err == nil {
		t.Fatal("cross-workspace dup accepted")
	}
	if _, err := m.SetHostnames("w1", nil); err != nil {
		t.Fatalf("clear: %v", err)
	}
	if _, err := m.SetHostnames("w2", []model.Hostname{{Name: "chartlabs-app", Port: 4000}}); err != nil {
		t.Fatalf("reclaim after release: %v", err)
	}

	// AllHostnames reflects the final state.
	if got := m.AllHostnames(); !reflect.DeepEqual(got, map[string]int{"chartlabs-app": 4000}) {
		t.Fatalf("AllHostnames = %v", got)
	}
}

// TestKillPane pins the generic close-a-pane path (a hosted tab's ✕ in a lens):
// the pane leaves the workspace and the store; a pane the workspace doesn't
// hold is a no-op and an unknown workspace errors.
func TestKillPane(t *testing.T) {
	m, st := devhostManager(t)
	p := &model.Pane{ID: "pane-1", WorkspaceID: "w1", CWD: "/r1"}
	if err := st.SavePane(p); err != nil {
		t.Fatal(err)
	}
	m.mu.Lock()
	m.byID["w1"].ws.Panes = append(m.byID["w1"].ws.Panes, p)
	m.mu.Unlock()

	if err := m.KillPane("w1", "pane-1"); err != nil {
		t.Fatalf("kill: %v", err)
	}
	if got := m.Workspace("w1").Panes; len(got) != 0 {
		t.Fatalf("panes = %+v, want none", got)
	}
	loaded, err := st.Load()
	if err != nil {
		t.Fatal(err)
	}
	for _, l := range loaded {
		if l.ID == "w1" && len(l.Panes) != 0 {
			t.Fatalf("persisted panes = %+v, want none", l.Panes)
		}
	}

	if err := m.KillPane("w1", "pane-1"); err != nil {
		t.Fatalf("second kill should be a no-op, got %v", err)
	}
	if err := m.KillPane("nope", "pane-1"); !errors.Is(err, ErrUnknownWorkspace) {
		t.Fatalf("err = %v, want ErrUnknownWorkspace", err)
	}
}

// TestLensHostname pins the reserved-label invariant both ways: the lens can't
// take a workspace's name, and a workspace can't take the lens's.
func TestLensHostname(t *testing.T) {
	m, _ := devhostManager(t)

	if err := m.SetLensHostname("CCMux"); err != nil {
		t.Fatalf("set: %v", err)
	}
	if got := m.LensHostname(); got != "ccmux" {
		t.Fatalf("lens = %q, want normalized ccmux", got)
	}
	// A workspace claim of the lens label is rejected.
	if _, err := m.SetHostnames("w1", []model.Hostname{{Name: "ccmux", Port: 3000}}); err == nil {
		t.Fatal("workspace claim of the lens label must be rejected")
	}
	// The lens can't take a label a workspace already maps.
	if _, err := m.SetHostnames("w1", []model.Hostname{{Name: "app", Port: 3000}}); err != nil {
		t.Fatalf("workspace hostname: %v", err)
	}
	if err := m.SetLensHostname("app"); err == nil {
		t.Fatal("lens label colliding with a workspace hostname must be rejected")
	}
	// Bad labels are rejected; "" clears.
	if err := m.SetLensHostname("no.dots"); err == nil {
		t.Fatal("dotted lens label must be rejected")
	}
	if err := m.SetLensHostname(""); err != nil {
		t.Fatalf("clear: %v", err)
	}
	if got := m.LensHostname(); got != "" {
		t.Fatalf("lens after clear = %q", got)
	}
	// With the lens cleared, the label is claimable again.
	if _, err := m.SetHostnames("w2", []model.Hostname{{Name: "ccmux", Port: 3100}}); err != nil {
		t.Fatalf("claim after clear: %v", err)
	}
}

func TestSetHostnames_UnknownWorkspace(t *testing.T) {
	m, _ := devhostManager(t)
	_, err := m.SetHostnames("nope", []model.Hostname{{Name: "app", Port: 1}})
	if !errors.Is(err, ErrUnknownWorkspace) {
		t.Fatalf("err = %v, want ErrUnknownWorkspace", err)
	}
}

// TestPortSuggestions pins the sheet-prepopulation flow: detected service
// labels merge with the repo slug, and rows colliding with existing mappings
// (name anywhere, port in this workspace) are dropped.
func TestPortSuggestions(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "reg.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	m := New(context.Background(), &tmux.Server{Socket: "unused"}, st)

	// Fixture repo shaped like the admin monorepo: compose names two services.
	repo := filepath.Join(t.TempDir(), "admin")
	if err := os.MkdirAll(repo, 0o755); err != nil {
		t.Fatal(err)
	}
	compose := "services:\n  web:\n    ports: [\"3001:3001\"]\n  api:\n    ports: [\"8001:8001\"]\n"
	if err := os.WriteFile(filepath.Join(repo, "docker-compose.yml"), []byte(compose), 0o644); err != nil {
		t.Fatal(err)
	}
	ws := &model.Workspace{ID: "w1", Name: "admin", RepoPath: repo}
	if err := st.SaveWorkspace(ws); err != nil {
		t.Fatal(err)
	}
	m.adopt(ws, false)

	got, err := m.PortSuggestions("w1")
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]int{"admin-api": 8001, "admin": 3001} // "web" folds into the slug
	if len(got) != 2 || got[0].Port != want[got[0].Name] || got[1].Port != want[got[1].Name] {
		t.Fatalf("suggestions = %+v, want %v", got, want)
	}

	// Mapping one of the ports removes that suggestion; a taken name uniquifies.
	if _, err := m.SetHostnames("w1", []model.Hostname{{Name: "admin", Port: 8001}}); err != nil {
		t.Fatal(err)
	}
	got, err = m.PortSuggestions("w1")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Port != 3001 || got[0].Name != "admin-3001" {
		t.Fatalf("after mapping = %+v, want [admin-3001:3001]", got)
	}

	if _, err := m.PortSuggestions("nope"); !errors.Is(err, ErrUnknownWorkspace) {
		t.Fatalf("unknown ws err = %v", err)
	}

	// A repo-level detection (empty service label) suggests the bare slug —
	// model.Slug's "repo" fallback must not leak in ("website-repo").
	repo2 := filepath.Join(t.TempDir(), "website")
	if err := os.MkdirAll(repo2, 0o755); err != nil {
		t.Fatal(err)
	}
	pkg := `{"scripts": {"dev": "next dev -p 3003"}}`
	if err := os.WriteFile(filepath.Join(repo2, "package.json"), []byte(pkg), 0o644); err != nil {
		t.Fatal(err)
	}
	ws2 := &model.Workspace{ID: "w9", Name: "website", RepoPath: repo2}
	if err := st.SaveWorkspace(ws2); err != nil {
		t.Fatal(err)
	}
	m.adopt(ws2, false)
	got, err = m.PortSuggestions("w9")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Name != "website" || got[0].Port != 3003 {
		t.Fatalf("repo-level suggestion = %+v, want website:3003", got)
	}
}

// TestDevCommand pins resolution (override beats detection), persistence, and
// the cold-workspace start/stop edges (spawn requires a live tmux session;
// stop without a running pane is a no-op).
func TestDevCommand(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "reg.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	m := New(context.Background(), &tmux.Server{Socket: "unused"}, st)

	repo := filepath.Join(t.TempDir(), "website")
	if err := os.MkdirAll(repo, 0o755); err != nil {
		t.Fatal(err)
	}
	pkg := `{"scripts": {"dev": "next dev -p 3003"}}`
	if err := os.WriteFile(filepath.Join(repo, "package.json"), []byte(pkg), 0o644); err != nil {
		t.Fatal(err)
	}
	ws := &model.Workspace{ID: "w1", Name: "website", RepoPath: repo}
	if err := st.SaveWorkspace(ws); err != nil {
		t.Fatal(err)
	}
	m.adopt(ws, false)

	// Detection (no lockfile → npm), then override, then back to detection.
	if cmd, src, _ := m.ResolveDevCommand("w1"); cmd != "npm run dev" || src != "package.json" {
		t.Fatalf("detected = %q/%q", cmd, src)
	}
	if err := m.SetDevCommand("w1", "make dev"); err != nil {
		t.Fatal(err)
	}
	if cmd, src, _ := m.ResolveDevCommand("w1"); cmd != "make dev" || src != "workspace setting" {
		t.Fatalf("override = %q/%q", cmd, src)
	}
	loaded, _ := st.Load()
	if loaded[0].DevCommand != "make dev" {
		t.Fatalf("persisted = %q", loaded[0].DevCommand)
	}
	if err := m.SetDevCommand("w1", ""); err != nil {
		t.Fatal(err)
	}
	if cmd, _, _ := m.ResolveDevCommand("w1"); cmd != "npm run dev" {
		t.Fatalf("after clear = %q", cmd)
	}

	// Start on a cold workspace fails at the spawn (no live tmux session);
	// stop is an idempotent no-op.
	if _, err := m.StartDevServer("w1"); err == nil {
		t.Fatal("start on cold workspace should fail")
	}
	if _, err := m.StopDevServer("w1"); err != nil {
		t.Fatalf("stop without pane: %v", err)
	}
	if _, err := m.StartDevServer("nope"); !errors.Is(err, ErrUnknownWorkspace) {
		t.Fatalf("unknown ws start err = %v", err)
	}
}

func TestStampHostnameRuntime(t *testing.T) {
	m, _ := devhostManager(t)
	if _, err := m.SetHostnames("w1", []model.Hostname{{Name: "app", Port: 3001}}); err != nil {
		t.Fatal(err)
	}
	m.StampHostnameRuntime(
		func(name string) string { return "https://" + name + ".dev.sanlabs.io" },
		func(port int) bool { return port == 3001 },
	)
	ws := m.Workspace("w1")
	h := ws.Hostnames[0]
	if h.URL != "https://app.dev.sanlabs.io" || !h.Listening {
		t.Fatalf("stamped = %+v", h)
	}
}
