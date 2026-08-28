package manager

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"strings"
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

// TestSetHostnames_AllocatesPortZero pins the allocated-port model: a row saved
// with port 0 gets a port from the daemon's reserved range, its targetPort
// survives the store round-trip, and a second allocation skips the first port
// even from another workspace.
func TestSetHostnames_AllocatesPortZero(t *testing.T) {
	m, st := devhostManager(t)

	ws, err := m.SetHostnames("w1", []model.Hostname{{Name: "app", Port: 0, TargetPort: 3000}})
	if err != nil {
		t.Fatalf("set: %v", err)
	}
	got := ws.Hostnames[0]
	if got.Port < devPortBase || got.Port > devPortMax {
		t.Fatalf("allocated port %d outside %d-%d", got.Port, devPortBase, devPortMax)
	}
	loaded, err := st.Load()
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	for _, l := range loaded {
		if l.ID == "w1" && (len(l.Hostnames) != 1 || l.Hostnames[0].Port != got.Port || l.Hostnames[0].TargetPort != 3000) {
			t.Fatalf("persisted = %+v", l.Hostnames)
		}
	}

	// A second auto row, in another workspace, must not reuse w1's port.
	ws2, err := m.SetHostnames("w2", []model.Hostname{{Name: "api", Port: 0}})
	if err != nil {
		t.Fatalf("set w2: %v", err)
	}
	if ws2.Hostnames[0].Port == got.Port {
		t.Fatalf("both workspaces allocated %d", got.Port)
	}

	// Round-trip: re-saving the assigned port keeps it (no reallocation).
	again, err := m.SetHostnames("w1", []model.Hostname{{Name: "app", Port: got.Port, TargetPort: 3000}})
	if err != nil {
		t.Fatalf("resave: %v", err)
	}
	if again.Hostnames[0].Port != got.Port {
		t.Fatalf("resave moved the port: %d → %d", got.Port, again.Hostnames[0].Port)
	}
}

// TestSetHostnames_BlankPortBackfillsTargetPort pins the migration path for
// pre-allocation rows: blanking the port moves the old port (which was the
// detected app port) into targetPort, so the compose override keeps working.
// An old port inside the reserved range is a ccmux allocation and must NOT be
// carried over.
func TestSetHostnames_BlankPortBackfillsTargetPort(t *testing.T) {
	m, _ := devhostManager(t)

	// A pre-allocation row: port 3000, no targetPort.
	if _, err := m.SetHostnames("w1", []model.Hostname{{Name: "app", Port: 3000}}); err != nil {
		t.Fatal(err)
	}
	ws, err := m.SetHostnames("w1", []model.Hostname{{Name: "app", Port: 0}})
	if err != nil {
		t.Fatal(err)
	}
	got := ws.Hostnames[0]
	if got.TargetPort != 3000 {
		t.Fatalf("targetPort = %d, want the old port 3000", got.TargetPort)
	}
	if got.Port < devPortBase || got.Port > devPortMax {
		t.Fatalf("port %d not allocated from the reserved range", got.Port)
	}

	// Blanking an already-allocated row must not backfill its 21xxx port.
	ws, err = m.SetHostnames("w1", []model.Hostname{{Name: "app", Port: 0}})
	if err != nil {
		t.Fatal(err)
	}
	if ws.Hostnames[0].TargetPort != 0 {
		t.Fatalf("targetPort = %d, want 0 (allocated ports are not app ports)", ws.Hostnames[0].TargetPort)
	}
}

// TestAllocateDevPort_SkipsSquatters pins the bind probe: a port in the
// reserved range that some process already listens on must be skipped, or the
// hostname routes to a stranger's server with a green dot.
func TestAllocateDevPort_SkipsSquatters(t *testing.T) {
	m, _ := devhostManager(t)
	// Squat the first port of the range. If Listen fails, something else
	// already holds it — squatted either way, the assertion below stands.
	if l, err := net.Listen("tcp", "127.0.0.1:21000"); err == nil {
		defer l.Close()
	}
	ws, err := m.SetHostnames("w1", []model.Hostname{{Name: "app", Port: 0}})
	if err != nil {
		t.Fatal(err)
	}
	if ws.Hostnames[0].Port == 21000 {
		t.Fatal("allocated the squatted port 21000")
	}
}

// TestPortSuggestions_TargetPortDedupes pins the allocated-port model's dedup:
// a saved row covers its detected port via TargetPort (its routing Port is
// 21xxx), and the sheet must not re-suggest that service forever.
func TestPortSuggestions_TargetPortDedupes(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "reg.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	m := New(context.Background(), &tmux.Server{Socket: "unused"}, st)
	repo := t.TempDir()
	compose := "services:\n  web:\n    ports: [\"3001:3001\"]\n  api:\n    ports: [\"8001:8001\"]\n"
	if err := os.WriteFile(filepath.Join(repo, "docker-compose.yml"), []byte(compose), 0o644); err != nil {
		t.Fatal(err)
	}
	ws := &model.Workspace{ID: "w1", Name: "admin", RepoPath: repo}
	if err := st.SaveWorkspace(ws); err != nil {
		t.Fatal(err)
	}
	m.adopt(ws, false)

	if _, err := m.SetHostnames("w1", []model.Hostname{{Name: "admin-api", Port: 0, TargetPort: 8001}}); err != nil {
		t.Fatal(err)
	}
	got, err := m.PortSuggestions("w1")
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range got {
		if s.Port == 8001 {
			t.Fatalf("already-mapped target port re-suggested: %+v", got)
		}
	}
	if len(got) != 1 || got[0].Port != 3001 {
		t.Fatalf("the unmapped service should still be suggested: %+v", got)
	}
}

// TestDevEnv_PortAndComposeFile pins the injection contract: exactly one
// mapped hostname puts PORT/CCMUX_DEV_PORT in pane env; a compose repo gets
// COMPOSE_FILE listing the repo's compose file plus the generated override;
// a second hostname drops PORT (two apps reading one PORT would collide).
func TestDevEnv_PortAndComposeFile(t *testing.T) {
	m, _ := devhostManager(t)
	repo := t.TempDir()
	compose := filepath.Join(repo, "docker-compose.yml")
	if err := os.WriteFile(compose, []byte("services:\n  web:\n    ports:\n      - \"3000:3000\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	m.DevhostDir = t.TempDir()
	m.mu.Lock()
	m.byID["w1"].ws.RepoPath = repo
	m.mu.Unlock()

	ws, err := m.SetHostnames("w1", []model.Hostname{{Name: "app", Port: 0, TargetPort: 3000}})
	if err != nil {
		t.Fatal(err)
	}
	port := ws.Hostnames[0].Port
	env := m.devEnv(ws)
	if env["PORT"] == "" || env["PORT"] != env["CCMUX_DEV_PORT"] {
		t.Fatalf("PORT env = %q / %q", env["PORT"], env["CCMUX_DEV_PORT"])
	}
	cf := env["COMPOSE_FILE"]
	if !strings.HasPrefix(cf, compose+":") || !strings.Contains(cf, "w1.yml") {
		t.Fatalf("COMPOSE_FILE = %q", cf)
	}
	overridePath := filepath.Join(m.DevhostDir, "compose", "w1.yml")
	raw, err := os.ReadFile(overridePath)
	if err != nil {
		t.Fatalf("override not written: %v", err)
	}
	if want := fmt.Sprintf("%d:3000", port); !strings.Contains(string(raw), want) {
		t.Fatalf("override lacks %q:\n%s", want, raw)
	}

	// Two hostnames: PORT is ambiguous and must go; compose remapping stays.
	ws, err = m.SetHostnames("w1", []model.Hostname{
		{Name: "app", Port: port, TargetPort: 3000},
		{Name: "api", Port: 0},
	})
	if err != nil {
		t.Fatal(err)
	}
	env = m.devEnv(ws)
	if _, ok := env["PORT"]; ok {
		t.Fatalf("PORT should be dropped with two hostnames, env = %v", env)
	}
	if env["COMPOSE_FILE"] == "" {
		t.Fatal("COMPOSE_FILE should survive with two hostnames")
	}

	// Clearing the mappings removes the stale override file.
	if _, err := m.SetHostnames("w1", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(overridePath); !os.IsNotExist(err) {
		t.Fatalf("stale override survived: %v", err)
	}
}

// givePanes attaches panes to a fixture workspace in both memory and the store.
func givePanes(t *testing.T, m *Manager, st *store.SQLite, wsID string, ids ...string) {
	t.Helper()
	for _, id := range ids {
		p := &model.Pane{ID: id, WorkspaceID: wsID, CWD: "/r1"}
		if err := st.SavePane(p); err != nil {
			t.Fatal(err)
		}
		m.mu.Lock()
		m.byID[wsID].ws.Panes = append(m.byID[wsID].ws.Panes, p)
		m.mu.Unlock()
	}
}

// persistedPanes reads a workspace's pane rows back out of the store.
func persistedPanes(t *testing.T, st *store.SQLite, wsID string) []*model.Pane {
	t.Helper()
	loaded, err := st.Load()
	if err != nil {
		t.Fatal(err)
	}
	for _, l := range loaded {
		if l.ID == wsID {
			return l.Panes
		}
	}
	t.Fatalf("workspace %s not in store", wsID)
	return nil
}

// TestKillPane pins the generic close-a-pane path (a hosted tab's ✕ in a lens):
// the pane leaves the workspace and the store; a pane the workspace doesn't
// hold is a no-op and an unknown workspace errors.
func TestKillPane(t *testing.T) {
	m, st := devhostManager(t)
	givePanes(t, m, st, "w1", "pane-1", "pane-2")

	if err := m.KillPane("w1", "pane-1"); err != nil {
		t.Fatalf("kill: %v", err)
	}
	got := m.Workspace("w1").Panes
	if len(got) != 1 || got[0].ID != "pane-2" {
		t.Fatalf("panes = %+v, want only pane-2", got)
	}
	if p := persistedPanes(t, st, "w1"); len(p) != 1 || p[0].ID != "pane-2" {
		t.Fatalf("persisted panes = %+v, want only pane-2", p)
	}

	if err := m.KillPane("w1", "pane-1"); err != nil {
		t.Fatalf("second kill should be a no-op, got %v", err)
	}
	if err := m.KillPane("nope", "pane-1"); !errors.Is(err, ErrUnknownWorkspace) {
		t.Fatalf("err = %v, want ErrUnknownWorkspace", err)
	}
}

// TestKillLastPaneArchives pins the fix for the un-revivable zero-pane row:
// closing the final pane ends the session (cold) but KEEPS the pane recipe, so
// the workspace still revives. Dropping the row instead left a cold entry that
// ReviveWorkspace rejects with "has no panes to revive" and that no lens could
// clear except Remove Session.
func TestKillLastPaneArchives(t *testing.T) {
	m, st := devhostManager(t)
	givePanes(t, m, st, "w1", "only-pane")

	if err := m.KillPane("w1", "only-pane"); err != nil {
		t.Fatalf("kill: %v", err)
	}

	ws := m.Workspace("w1")
	if ws == nil {
		t.Fatal("workspace deleted; archiving must keep the row")
	}
	if ws.Status != model.StatusCold {
		t.Fatalf("status = %q, want %q", ws.Status, model.StatusCold)
	}
	if len(ws.Panes) != 1 || ws.Panes[0].ID != "only-pane" {
		t.Fatalf("panes = %+v, want the recipe kept", ws.Panes)
	}
	if p := persistedPanes(t, st, "w1"); len(p) != 1 || p[0].ID != "only-pane" {
		t.Fatalf("persisted panes = %+v, want the recipe kept", p)
	}
	// Holding a pane is exactly ReviveWorkspace's precondition (manager.go: "has no
	// panes to revive"), so the assertions above are what keep the row revivable.
	// Revive itself isn't called here — it would exec tmux against the fixture socket.
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

// TestStartDevServer_ExistingPaneDecisions pins restartDevServer's no-op
// branches on a cold fixture (no controller): a listening server or a busy
// pane must not be touched, and a dead-idle pane without a controller must
// no-op rather than crash — in every case the recorded startup command stays.
func TestStartDevServer_ExistingPaneDecisions(t *testing.T) {
	m, st := devhostManager(t)
	givePanes(t, m, st, "w1", "dev-pane")
	m.mu.Lock()
	p := m.byID["w1"].ws.Panes[0]
	p.DevServer = true
	p.StartupCommand = "npm run dev"
	m.mu.Unlock()

	set := func(raw string, listening bool) {
		m.mu.Lock()
		p.RawCommand = raw
		m.byID["w1"].ws.Hostnames = []model.Hostname{{Name: "app", Port: 21000, Listening: listening}}
		m.mu.Unlock()
	}
	for _, tc := range []struct {
		name      string
		raw       string
		listening bool
	}{
		{"listening server", "node", true},
		{"busy pane still starting", "node", false},
		{"dead idle pane, no controller", "zsh", false},
	} {
		set(tc.raw, tc.listening)
		ws, err := m.StartDevServer("w1")
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if len(ws.Panes) != 1 || ws.Panes[0].StartupCommand != "npm run dev" {
			t.Fatalf("%s: pane mutated: %+v", tc.name, ws.Panes)
		}
	}
}

// TestStopDevServerRefusesWhenItIsTheOnlyPane guards the seam between "stop the
// dev server" and KillPane's last-pane archive: without the refusal, ■ would kill
// the tmux session and still answer 200, so every layer would report success while
// the user's session went cold.
func TestStopDevServerRefusesWhenItIsTheOnlyPane(t *testing.T) {
	m, st := devhostManager(t)
	givePanes(t, m, st, "w1", "dev-pane")
	m.mu.Lock()
	m.byID["w1"].ws.Panes[0].DevServer = true
	m.mu.Unlock()

	if _, err := m.StopDevServer("w1"); err == nil {
		t.Fatal("stop should refuse to close the session's only pane")
	}
	// The pane must survive — a refusal that still killed something is not a refusal.
	if ws := m.Workspace("w1"); len(ws.Panes) != 1 || ws.Panes[0].ID != "dev-pane" {
		t.Fatalf("panes = %+v, want the dev pane still there", ws.Panes)
	}
	if p := persistedPanes(t, st, "w1"); len(p) != 1 {
		t.Fatalf("persisted panes = %+v, want the dev pane still there", p)
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
