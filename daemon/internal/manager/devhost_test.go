package manager

import (
	"context"
	"errors"
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
