package api

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"ccmux.dev/ccmuxd/internal/manager"
	"ccmux.dev/ccmuxd/internal/store"
	"ccmux.dev/ccmuxd/internal/tmux"
)

// settingsServer builds a Server around a real store-backed manager (no tmux
// started — the settings paths never touch it).
func settingsServer(t *testing.T) *Server {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "reg.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	mgr := manager.New(context.Background(), &tmux.Server{Socket: "unused"}, st)
	return &Server{mgr: mgr}
}

func TestSettings_StartupCommandRoundtrip(t *testing.T) {
	s := settingsServer(t)

	get := func() string {
		rec := httptest.NewRecorder()
		s.getSettings(rec, httptest.NewRequest("GET", "/v1/settings", nil))
		var got struct {
			StartupCommand string `json:"startupCommand"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
			t.Fatalf("decode: %v", err)
		}
		return got.StartupCommand
	}
	put := func(body string) string {
		rec := httptest.NewRecorder()
		s.putSettings(rec, httptest.NewRequest("PUT", "/v1/settings", strings.NewReader(body)))
		if rec.Code != 200 {
			t.Fatalf("put = %d (%s)", rec.Code, rec.Body)
		}
		var got struct {
			StartupCommand string `json:"startupCommand"`
		}
		_ = json.Unmarshal(rec.Body.Bytes(), &got)
		return got.StartupCommand
	}

	// Unset → the peers-enabled fallback, resolved.
	if got := get(); got != manager.FallbackStartupCommand {
		t.Fatalf("default = %q, want fallback", got)
	}
	// Set a custom command → persisted and echoed.
	if got := put(`{"startupCommand":"claude --continue"}`); got != "claude --continue" {
		t.Fatalf("after put = %q", got)
	}
	if got := get(); got != "claude --continue" {
		t.Fatalf("get after put = %q", got)
	}
	// Empty (whitespace) resets to the fallback.
	if got := put(`{"startupCommand":"   "}`); got != manager.FallbackStartupCommand {
		t.Fatalf("after reset = %q, want fallback", got)
	}
	// PUT without the field changes nothing.
	put(`{"startupCommand":"claude --continue"}`)
	if got := put(`{}`); got != "claude --continue" {
		t.Fatalf("field-less put changed setting to %q", got)
	}
}

func TestSettings_StartupRulesResolution(t *testing.T) {
	s := settingsServer(t)

	rec := httptest.NewRecorder()
	s.putSettings(rec, httptest.NewRequest("PUT", "/v1/settings", strings.NewReader(`{
		"startupCommand": "claude",
		"startupRules": [
			{"pathPrefix": "/w/Coding/ChartLabs/", "command": "claude --chartlabs"},
			{"pathPrefix": "/w/Coding/ChartLabs/backend", "command": "claude --backend"},
			{"pathPrefix": "", "command": "dropped"},
			{"pathPrefix": "/w/half-filled", "command": "  "}
		]}`)))
	if rec.Code != 200 {
		t.Fatalf("put rules = %d (%s)", rec.Code, rec.Body)
	}
	var saved struct {
		StartupRules []manager.StartupRule `json:"startupRules"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &saved); err != nil {
		t.Fatal(err)
	}
	if len(saved.StartupRules) != 2 {
		t.Fatalf("rules = %+v, want empty rows dropped", saved.StartupRules)
	}

	resolve := func(repo string) string {
		rec := httptest.NewRecorder()
		s.getSettings(rec, httptest.NewRequest("GET", "/v1/settings?repoPath="+repo, nil))
		var got map[string]any
		_ = json.Unmarshal(rec.Body.Bytes(), &got)
		v, _ := got["resolvedStartupCommand"].(string)
		return v
	}
	// Longest matching prefix wins; boundary-aware (ChartLabsFoo ≠ ChartLabs/…).
	if got := resolve("/w/Coding/ChartLabs/admin"); got != "claude --chartlabs" {
		t.Fatalf("admin resolved %q", got)
	}
	if got := resolve("/w/Coding/ChartLabs/backend"); got != "claude --backend" {
		t.Fatalf("backend resolved %q", got)
	}
	if got := resolve("/w/Coding/ChartLabsFoo"); got != "claude" {
		t.Fatalf("sibling with shared name prefix resolved %q, want global", got)
	}
	if got := resolve("/w/Elsewhere/thing"); got != "claude" {
		t.Fatalf("unmatched resolved %q, want global", got)
	}
}
