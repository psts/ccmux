package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"strings"
	"testing"

	"ccmux.dev/ccmuxd/internal/harness"
	"ccmux.dev/ccmuxd/internal/manager"
	"ccmux.dev/ccmuxd/internal/store"
	"ccmux.dev/ccmuxd/internal/tmux"
)

// harnessStack is floodStack plus the harness registry wired the way main
// wires it.
func harnessStack(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux not installed")
	}
	tsrv := &tmux.Server{Socket: "ccmux-harness-itest", ConfigPath: "../../config/tmux.conf"}
	_ = tsrv.KillServer()
	t.Cleanup(func() { _ = tsrv.KillServer() })

	st, err := store.Open(t.TempDir() + "/reg.db")
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	mgr := manager.New(ctx, tsrv, st)
	mgr.Harnesses = harness.New(st, mgr.DefaultStartupCommand)
	if err := mgr.Start(); err != nil {
		t.Fatalf("manager start: %v", err)
	}
	hs := httptest.NewServer(NewServer(mgr).Handler())
	t.Cleanup(hs.Close)
	return hs.URL
}

func TestSpawnPaneByHarness(t *testing.T) {
	base := harnessStack(t)
	ws := createWS(t, base)

	// Configure a harmless harness, then spawn a pane by its name.
	rec, err := http.NewRequest("PUT", base+"/v1/settings",
		strings.NewReader(`{"harnesses":[{"name":"noop","icon":"·","command":": noop-harness"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	if resp, err := http.DefaultClient.Do(rec); err != nil || resp.StatusCode != 200 {
		t.Fatalf("put harnesses: %v %v", err, resp.StatusCode)
	}

	resp, err := http.Post(base+"/v1/workspaces/"+ws.ID+"/panes", "application/json",
		strings.NewReader(`{"harness":"noop","createdBy":"tester"}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var pane struct {
		ID             string `json:"id"`
		Harness        string `json:"harness"`
		StartupCommand string `json:"startupCommand"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&pane); err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != 201 || pane.Harness != "noop" || pane.StartupCommand != ": noop-harness" {
		t.Fatalf("spawn = %d %+v, want the harness recorded and its command persisted", resp.StatusCode, pane)
	}

	// Unknown harness → 400; harness + startupCommand together → 400.
	for _, body := range []string{
		`{"harness":"ghost"}`,
		`{"harness":"noop","startupCommand":"ls"}`,
	} {
		r, err := http.Post(base+"/v1/workspaces/"+ws.ID+"/panes", "application/json", strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		r.Body.Close()
		if r.StatusCode != http.StatusBadRequest {
			t.Fatalf("spawn %s = %d, want 400", body, r.StatusCode)
		}
	}
}

// The settings surface lists the resolved set, built-in claude included.
func TestHarnessSettingsListsBuiltin(t *testing.T) {
	base := harnessStack(t)

	resp, err := http.Get(base + "/v1/settings")
	if err != nil {
		t.Fatal(err)
	}
	var cfg struct {
		Harnesses []harness.Harness `json:"harnesses"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&cfg)
	resp.Body.Close()
	if len(cfg.Harnesses) != 1 || cfg.Harnesses[0].Name != "claude" {
		t.Fatalf("harnesses = %+v, want built-in claude", cfg.Harnesses)
	}
}
