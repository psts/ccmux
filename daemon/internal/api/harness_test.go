package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"strings"
	"testing"
	"time"

	"ccmux.dev/ccmuxd/internal/harness"
	"ccmux.dev/ccmuxd/internal/llmproxy"
	"ccmux.dev/ccmuxd/internal/manager"
	"ccmux.dev/ccmuxd/internal/store"
	"ccmux.dev/ccmuxd/internal/tmux"
)

// harnessStack is floodStack plus the harness registry wired the way main
// wires it.
func harnessStack(t *testing.T) (*manager.Manager, string) {
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
	srv := NewServer(mgr)
	// Wired the way main wires it: the llm proxy serves the settings surface
	// AND the manager's harness-pairing route setter.
	llm := llmproxy.New(st)
	srv.SetLLMProxy(llm)
	mgr.PaneLLMRoute = llm.SetPaneRoute
	hs := httptest.NewServer(srv.Handler())
	t.Cleanup(hs.Close)
	return mgr, hs.URL
}

func TestSpawnPaneByHarness(t *testing.T) {
	_, base := harnessStack(t)
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
	_, base := harnessStack(t)

	resp, err := http.Get(base + "/v1/settings")
	if err != nil {
		t.Fatal(err)
	}
	var cfg struct {
		Harnesses []harness.Harness `json:"harnesses"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&cfg)
	resp.Body.Close()
	// Detected entries are host-dependent (pi/opencode may be installed on the
	// build host); the invariant is claude first, as the builtin.
	if len(cfg.Harnesses) == 0 || cfg.Harnesses[0].Name != "claude" || cfg.Harnesses[0].Source != "builtin" {
		t.Fatalf("harnesses = %+v, want built-in claude first", cfg.Harnesses)
	}
}

// Starting a harness IN an existing shell pane records it and delivers its
// command; a busy pane refuses with 409, and the folder-rule preselection
// rides the settings response.
func TestStartHarnessInPane(t *testing.T) {
	mgr, base := harnessStack(t)
	ws := createWS(t, base)
	pane := ws.Panes[0].ID

	// Wait for tmux's command signal so the daemon knows the pane is a shell.
	deadline := time.Now().Add(5 * time.Second)
	for !mgr.PaneAtShell(pane) {
		if time.Now().After(deadline) {
			t.Fatal("pane never reported a bare shell")
		}
		time.Sleep(50 * time.Millisecond)
	}

	put, _ := http.NewRequest("PUT", base+"/v1/settings",
		strings.NewReader(`{"harnesses":[{"name":"sleeper","command":"sleep 5"}]}`))
	if resp, err := http.DefaultClient.Do(put); err != nil || resp.StatusCode != 200 {
		t.Fatalf("put harnesses: %v %v", err, resp.StatusCode)
	}

	start := func() *http.Response {
		r, err := http.Post(base+"/v1/panes/"+pane+"/harness", "application/json",
			strings.NewReader(`{"harness":"sleeper"}`))
		if err != nil {
			t.Fatal(err)
		}
		r.Body.Close()
		return r
	}
	if r := start(); r.StatusCode != 200 {
		t.Fatalf("start = %d, want 200", r.StatusCode)
	}

	// The command signal flips the pane to busy; a second start must refuse.
	deadline = time.Now().Add(5 * time.Second)
	for mgr.PaneAtShell(pane) {
		if time.Now().After(deadline) {
			t.Fatal("pane never reported the sleeper running")
		}
		time.Sleep(50 * time.Millisecond)
	}
	if r := start(); r.StatusCode != http.StatusConflict {
		t.Fatalf("start on busy pane = %d, want 409", r.StatusCode)
	}

	// The pane recorded what it runs, and the recipe revives it.
	resp, err := http.Get(base + "/v1/workspaces")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var list []struct {
		ID    string `json:"id"`
		Panes []struct {
			ID, Harness, StartupCommand string
		} `json:"panes"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&list)
	for _, w := range list {
		if w.ID != ws.ID {
			continue
		}
		if w.Panes[0].Harness != "sleeper" || w.Panes[0].StartupCommand != "sleep 5" {
			t.Fatalf("pane = %+v, want harness and recipe recorded", w.Panes[0])
		}
	}

	// Preselection: /tmp has no folder rule, so the default (claude) answers.
	resp2, err := http.Get(base + "/v1/settings?repoPath=/tmp")
	if err != nil {
		t.Fatal(err)
	}
	defer resp2.Body.Close()
	var cfg struct {
		ResolvedHarness string `json:"resolvedHarness"`
	}
	_ = json.NewDecoder(resp2.Body).Decode(&cfg)
	if cfg.ResolvedHarness != "claude" {
		t.Fatalf("resolvedHarness = %q, want claude", cfg.ResolvedHarness)
	}
}

// The codex harness can only talk to a ChatGPT-backed account: without one
// the spawn refuses, with one the new pane's llm route points at it, and a
// non-codex harness starting in a codex-routed pane clears the override.
func TestCodexHarnessPairsPaneRoute(t *testing.T) {
	mgr, base := harnessStack(t)
	ws := createWS(t, base)

	putSettings := func(body string) {
		t.Helper()
		req, _ := http.NewRequest("PUT", base+"/v1/settings", strings.NewReader(body))
		resp, err := http.DefaultClient.Do(req)
		if err != nil || resp.StatusCode != 200 {
			t.Fatalf("put settings %s: %v %d", body, err, resp.StatusCode)
		}
	}
	// A user entry named codex: pairing keys on the NAME, so an override
	// (here: a harmless command) must keep it.
	putSettings(`{"harnesses":[{"name":"codex","command":": codex"},{"name":"noop","command":": noop"}]}`)

	spawn := func() *http.Response {
		r, err := http.Post(base+"/v1/workspaces/"+ws.ID+"/panes", "application/json",
			strings.NewReader(`{"harness":"codex","createdBy":"tester"}`))
		if err != nil {
			t.Fatal(err)
		}
		return r
	}
	// No codex-kind account configured → refused, pane never created.
	r := spawn()
	r.Body.Close()
	if r.StatusCode != http.StatusConflict {
		t.Fatalf("spawn without codex account = %d, want 409", r.StatusCode)
	}

	putSettings(`{"llmAccounts":[{"name":"cx","kind":"codex"}]}`)
	r = spawn()
	defer r.Body.Close()
	if r.StatusCode != 201 {
		t.Fatalf("spawn with codex account = %d, want 201", r.StatusCode)
	}
	var pane struct {
		ID string `json:"id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&pane); err != nil {
		t.Fatal(err)
	}
	routeOf := func(paneID string) string {
		t.Helper()
		resp, err := http.Get(base + "/v1/panes/" + paneID + "/llm-route")
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var got struct {
			Route string `json:"route"`
		}
		_ = json.NewDecoder(resp.Body).Decode(&got)
		return got.Route
	}
	if got := routeOf(pane.ID); got != "cx" {
		t.Fatalf("codex pane route = %q, want cx", got)
	}

	// Dialect guard: a codex pane refuses a non-codex account, and a named
	// non-codex harness refuses a codex account — at route-set time, with a
	// message, instead of as 404s in the pane.
	putSettings(`{"llmAccounts":[{"name":"cx","kind":"codex"},{"name":"local","kind":"anthropic","baseURL":"http://localhost:11434"}]}`)
	setRoute := func(paneID, route string) *http.Response {
		t.Helper()
		req, _ := http.NewRequest("PUT", base+"/v1/panes/"+paneID+"/llm-route",
			strings.NewReader(`{"route":"`+route+`"}`))
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp
	}
	if r := setRoute(pane.ID, "local"); r.StatusCode != http.StatusBadRequest {
		t.Fatalf("codex pane routed to anthropic account = %d, want 400", r.StatusCode)
	}
	// The picker itself hides the impossible: a codex pane is offered only
	// codex accounts.
	rr, err := http.Get(base + "/v1/panes/" + pane.ID + "/llm-route")
	if err != nil {
		t.Fatal(err)
	}
	var picker struct {
		Accounts []string `json:"accounts"`
	}
	_ = json.NewDecoder(rr.Body).Decode(&picker)
	rr.Body.Close()
	if len(picker.Accounts) != 1 || picker.Accounts[0] != "cx" {
		t.Fatalf("codex pane picker = %v, want only the codex account", picker.Accounts)
	}

	// A codex route left on a pane must be cleared when a NON-codex harness
	// starts there. Plant it the way it happens for real: start the codex
	// harness in the shell pane (pairing routes it to cx; the command exits
	// straight back to the shell), then start noop over it.
	shell := ws.Panes[0].ID
	waitShell := func() {
		t.Helper()
		deadline := time.Now().Add(5 * time.Second)
		for !mgr.PaneAtShell(shell) {
			if time.Now().After(deadline) {
				t.Fatal("pane never reported a bare shell")
			}
			time.Sleep(50 * time.Millisecond)
		}
	}
	startIn := func(name string) int {
		t.Helper()
		r, err := http.Post(base+"/v1/panes/"+shell+"/harness", "application/json",
			strings.NewReader(`{"harness":"`+name+`"}`))
		if err != nil {
			t.Fatal(err)
		}
		r.Body.Close()
		return r.StatusCode
	}
	waitShell()
	if code := startIn("codex"); code != 200 {
		t.Fatalf("start codex in shell pane = %d, want 200", code)
	}
	if got := routeOf(shell); got != "cx" {
		t.Fatalf("pane route after codex start = %q, want cx", got)
	}
	waitShell()
	if code := startIn("noop"); code != 200 {
		t.Fatalf("start noop = %d, want 200", code)
	}
	if got := routeOf(shell); got != "" {
		t.Fatalf("shell pane route after noop start = %q, want cleared", got)
	}
	// …and the reverse guard: the pane now runs noop, so a codex account is
	// refused for it.
	if r := setRoute(shell, "cx"); r.StatusCode != http.StatusBadRequest {
		t.Fatalf("noop pane routed to codex account = %d, want 400", r.StatusCode)
	}
}
