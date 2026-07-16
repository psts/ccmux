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
