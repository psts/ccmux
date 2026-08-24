package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os/exec"
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
