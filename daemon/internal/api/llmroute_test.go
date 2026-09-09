package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"strings"
	"testing"

	"ccmux.dev/ccmuxd/internal/llmproxy"
	"ccmux.dev/ccmuxd/internal/manager"
	"ccmux.dev/ccmuxd/internal/store"
	"ccmux.dev/ccmuxd/internal/tmux"
)

// llmRouteStack is floodStack plus the mounted proxy sharing the registry.
func llmRouteStack(t *testing.T) (*llmproxy.Service, string) {
	t.Helper()
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux not installed")
	}
	tsrv := &tmux.Server{Socket: "ccmux-llmroute-itest", ConfigPath: "../../config/tmux.conf"}
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
	if err := mgr.Start(); err != nil {
		t.Fatalf("manager start: %v", err)
	}
	llm := llmproxy.New(st)
	srv := NewServer(mgr)
	srv.SetLLMProxy(llm)
	hs := httptest.NewServer(srv.Handler())
	t.Cleanup(hs.Close)
	return llm, hs.URL
}

type paneRouteResp struct {
	Pane      string   `json:"pane"`
	Route     string   `json:"route"`
	Effective string   `json:"effective"`
	Accounts  []string `json:"accounts"`
}

func putRoute(t *testing.T, base, pane, route string) (*http.Response, paneRouteResp) {
	t.Helper()
	req, _ := http.NewRequest("PUT", base+"/v1/panes/"+pane+"/llm-route",
		bytes.NewReader([]byte(`{"route":"`+route+`"}`)))
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var got paneRouteResp
	_ = json.NewDecoder(resp.Body).Decode(&got)
	return resp, got
}

func TestPaneLLMRouteEndpoint(t *testing.T) {
	llm, base := llmRouteStack(t)
	ws := createWS(t, base)
	pane := ws.Panes[0].ID

	// Fresh pane: no override, effective = the built-in pass-through.
	resp, err := http.Get(base + "/v1/panes/" + pane + "/llm-route")
	if err != nil {
		t.Fatal(err)
	}
	var got paneRouteResp
	_ = json.NewDecoder(resp.Body).Decode(&got)
	resp.Body.Close()
	if resp.StatusCode != 200 || got.Route != "" || got.Effective != "anthropic" {
		t.Fatalf("fresh pane = %d %+v", resp.StatusCode, got)
	}

	// Unknown account → 400; unknown pane → 404.
	if r, _ := putRoute(t, base, pane, "ghost"); r.StatusCode != http.StatusBadRequest {
		t.Fatalf("unknown account = %d, want 400", r.StatusCode)
	}
	if r, _ := putRoute(t, base, "no-such-pane", ""); r.StatusCode != http.StatusNotFound {
		t.Fatalf("unknown pane = %d, want 404", r.StatusCode)
	}

	// With an account configured, the override sets, reports, and clears.
	accs := []llmproxy.Account{{Name: "ollama", BaseURL: "http://localhost:11434"}}
	if err := llm.Apply(&accs, nil); err != nil {
		t.Fatal(err)
	}
	r, set := putRoute(t, base, pane, "ollama")
	if r.StatusCode != 200 || set.Route != "ollama" || set.Effective != "ollama" || len(set.Accounts) != 1 {
		t.Fatalf("set = %d %+v", r.StatusCode, set)
	}
	r, cleared := putRoute(t, base, pane, "")
	if r.StatusCode != 200 || cleared.Route != "" || cleared.Effective != "anthropic" {
		t.Fatalf("clear = %d %+v", r.StatusCode, cleared)
	}
}

// The pane order the lenses render comes from the daemon resolved, because
// nothing in a lens can compute it: the pane's harness kinds, its account
// order and live account health all feed it. This pins that the settings
// answer carries it for every live pane, which is what lets the Mac lens show
// the chain without a fetch per menu.
func TestSettingsCarriesResolvedPaneOrders(t *testing.T) {
	mgr, base := harnessStack(t)
	ws := createWS(t, base)

	put := func(body string) {
		t.Helper()
		req, _ := http.NewRequest("PUT", base+"/v1/settings", strings.NewReader(body))
		resp, err := http.DefaultClient.Do(req)
		if err != nil || resp.StatusCode != 200 {
			t.Fatalf("put %s: %v", body, err)
		}
		resp.Body.Close()
	}
	put(`{"llmAccounts":[
		{"name":"a","kind":"anthropic","baseURL":"https://api.anthropic.com","apiKey":"k1"},
		{"name":"b","kind":"anthropic","baseURL":"https://api.anthropic.com","apiKey":"k2"}]}`)
	// claude's default kinds include anthropic, and this order reverses the
	// configured one — so the answer proves the harness order is what ran.
	put(`{"harnesses":[{"name":"claude","command":": claude","accountOrder":["b","a"]}]}`)

	// Spawned through the real path, so the pane records the harness the way
	// it does in production rather than through a test-only setter.
	r, err := http.Post(base+"/v1/workspaces/"+ws.ID+"/panes", "application/json",
		strings.NewReader(`{"harness":"claude","createdBy":"tester"}`))
	if err != nil {
		t.Fatal(err)
	}
	defer r.Body.Close()
	if r.StatusCode != 201 {
		t.Fatalf("spawn claude pane = %d, want 201", r.StatusCode)
	}
	var pane struct {
		ID string `json:"id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&pane); err != nil {
		t.Fatal(err)
	}
	paneID := pane.ID

	resp, err := http.Get(base + "/v1/settings")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var got struct {
		Orders map[string][]string `json:"llmPaneOrders"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatal(err)
	}
	if order := got.Orders[paneID]; strings.Join(order, ",") != "b,a" {
		t.Fatalf("pane order = %v, want [b a] — the harness's own order", order)
	}
	// And a pane with no harness follows the default account, not the
	// harness order — the two tiers must not bleed into each other.
	shell := mgr.List()[0].Panes[0].ID
	// Asserted POSITIVELY: a negative check here passed on an empty result,
	// so it stayed green if the field vanished or the endpoint failed — and
	// it would also have stayed green if tier 3 disappeared entirely.
	if order := got.Orders[shell]; strings.Join(order, ",") != "anthropic" {
		t.Fatalf("shell pane order = %v, want the synthetic pass-through", order)
	}
}
