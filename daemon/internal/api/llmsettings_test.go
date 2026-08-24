package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"ccmux.dev/ccmuxd/internal/llmproxy"
	"ccmux.dev/ccmuxd/internal/manager"
	"ccmux.dev/ccmuxd/internal/store"
	"ccmux.dev/ccmuxd/internal/tmux"
)

// llmServer is settingsServer plus the mounted proxy, sharing one registry.
func llmServer(t *testing.T) *Server {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "reg.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	mgr := manager.New(context.Background(), &tmux.Server{Socket: "unused"}, st)
	return &Server{mgr: mgr, llm: llmproxy.New(st)}
}

func TestSettings_LLMAccountsRedactedRoundtrip(t *testing.T) {
	s := llmServer(t)

	rec := httptest.NewRecorder()
	s.putSettings(rec, httptest.NewRequest("PUT", "/v1/settings", strings.NewReader(`{
		"llmAccounts": [
			{"name": "ollama", "baseURL": "http://localhost:11434"},
			{"name": "openrouter", "kind": "openai", "baseURL": "https://openrouter.ai/api", "apiKey": "or-secret"}
		],
		"llmRoute": "ollama"}`)))
	if rec.Code != 200 {
		t.Fatalf("put = %d (%s)", rec.Code, rec.Body)
	}
	if body := rec.Body.String(); strings.Contains(body, "or-secret") {
		t.Fatal("settings response echoed an api key")
	}
	var got struct {
		LLMAccounts []llmproxy.RedactedAccount `json:"llmAccounts"`
		LLMRoute    string                     `json:"llmRoute"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.LLMRoute != "ollama" || len(got.LLMAccounts) != 2 {
		t.Fatalf("roundtrip = %+v / %q", got.LLMAccounts, got.LLMRoute)
	}
	if !got.LLMAccounts[1].APIKeySet || got.LLMAccounts[0].APIKeySet {
		t.Fatalf("key presence wrong: %+v", got.LLMAccounts)
	}
}

func TestSettings_LLMRouteToMissingAccountIs400(t *testing.T) {
	s := llmServer(t)
	rec := httptest.NewRecorder()
	s.putSettings(rec, httptest.NewRequest("PUT", "/v1/settings", strings.NewReader(`{"llmRoute": "nope"}`)))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("put = %d, want 400", rec.Code)
	}
}

// A daemon without the proxy mounted must refuse llm fields, not answer 200
// while silently dropping them.
func TestSettings_LLMFieldsWithoutProxyAre400(t *testing.T) {
	s := settingsServer(t)
	rec := httptest.NewRecorder()
	s.putSettings(rec, httptest.NewRequest("PUT", "/v1/settings", strings.NewReader(`{"llmRoute": ""}`)))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("put = %d, want 400", rec.Code)
	}
}

// The mounted route strips the pane prefix, forwards to the routed account,
// and stays loopback-only.
func TestLLMRouteMountedAndLoopbackGuarded(t *testing.T) {
	var gotPath string
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
	}))
	defer up.Close()

	s := llmServer(t)
	rec := httptest.NewRecorder()
	s.putSettings(rec, httptest.NewRequest("PUT", "/v1/settings", strings.NewReader(
		`{"llmAccounts":[{"name":"test","baseURL":"`+up.URL+`"}],"llmRoute":"test"}`)))
	if rec.Code != 200 {
		t.Fatalf("put = %d (%s)", rec.Code, rec.Body)
	}
	h := s.Handler()

	req := httptest.NewRequest("POST", "/llm/pane/p1/v1/messages", strings.NewReader("{}"))
	req.RemoteAddr = "127.0.0.1:9999"
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 200 || gotPath != "/v1/messages" {
		t.Fatalf("loopback call = %d, upstream path %q", rec.Code, gotPath)
	}

	gotPath = ""
	req = httptest.NewRequest("POST", "/llm/pane/p1/v1/messages", strings.NewReader("{}"))
	req.RemoteAddr = "100.64.1.2:9999" // tailnet, not loopback
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code == 200 || gotPath != "" {
		t.Fatalf("non-loopback call = %d (upstream saw %q), want refused", rec.Code, gotPath)
	}
}
