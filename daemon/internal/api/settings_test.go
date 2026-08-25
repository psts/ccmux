package api

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"ccmux.dev/ccmuxd/internal/harness"
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
	mgr.Harnesses = harness.New(st)
	return &Server{mgr: mgr}
}

func TestSettings_HarnessRulesResolution(t *testing.T) {
	s := settingsServer(t)

	rec := httptest.NewRecorder()
	s.putSettings(rec, httptest.NewRequest("PUT", "/v1/settings", strings.NewReader(`{
		"harnesses": [{"name": "pi", "command": "pi"}, {"name": "opencode", "command": "opencode"}],
		"harnessRules": [
			{"pathPrefix": "/w/Coding/ChartLabs/", "harness": "pi"},
			{"pathPrefix": "/w/Coding/ChartLabs/backend", "harness": "opencode"},
			{"pathPrefix": "/w/gone", "harness": "deleted-harness"},
			{"pathPrefix": "", "harness": "dropped"},
			{"pathPrefix": "/w/half-filled", "harness": "  "}
		]}`)))
	if rec.Code != 200 {
		t.Fatalf("put rules = %d (%s)", rec.Code, rec.Body)
	}
	var saved struct {
		HarnessRules []harness.Rule `json:"harnessRules"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &saved); err != nil {
		t.Fatal(err)
	}
	if len(saved.HarnessRules) != 3 {
		t.Fatalf("rules = %+v, want empty rows dropped", saved.HarnessRules)
	}

	resolve := func(repo string) string {
		rec := httptest.NewRecorder()
		s.getSettings(rec, httptest.NewRequest("GET", "/v1/settings?repoPath="+repo, nil))
		var got map[string]any
		_ = json.Unmarshal(rec.Body.Bytes(), &got)
		v, _ := got["resolvedHarness"].(string)
		return v
	}
	// Longest matching prefix wins; boundary-aware (ChartLabsFoo ≠ ChartLabs/…);
	// every miss — including a rule whose harness no longer exists — suggests
	// the builtin.
	if got := resolve("/w/Coding/ChartLabs/admin"); got != "pi" {
		t.Fatalf("admin resolved %q", got)
	}
	if got := resolve("/w/Coding/ChartLabs/backend"); got != "opencode" {
		t.Fatalf("backend resolved %q", got)
	}
	if got := resolve("/w/Coding/ChartLabsFoo"); got != "claude" {
		t.Fatalf("sibling with shared name prefix resolved %q, want claude", got)
	}
	if got := resolve("/w/Elsewhere/thing"); got != "claude" {
		t.Fatalf("unmatched resolved %q, want claude", got)
	}
	if got := resolve("/w/gone/repo"); got != "claude" {
		t.Fatalf("dangling rule resolved %q, want claude", got)
	}
}

// An omitted harnessRules key means "leave the rules alone" — without this, a
// save of any unrelated setting from an editor would wipe every folder rule
// (the same contract the alias map pins for itself).
func TestSettings_OmittedHarnessRulesFieldLeavesRulesAlone(t *testing.T) {
	s := settingsServer(t)

	put := func(body string) int {
		rec := httptest.NewRecorder()
		s.putSettings(rec, httptest.NewRequest("PUT", "/v1/settings", strings.NewReader(body)))
		return rec.Code
	}
	if code := put(`{"harnessRules":[{"pathPrefix":"/w/CL","harness":"pi"}]}`); code != 200 {
		t.Fatalf("seed put = %d", code)
	}
	if code := put(`{"owner":"keep@example.com"}`); code != 200 {
		t.Fatalf("unrelated put = %d", code)
	}
	rec := httptest.NewRecorder()
	s.getSettings(rec, httptest.NewRequest("GET", "/v1/settings", nil))
	var got struct {
		HarnessRules []harness.Rule `json:"harnessRules"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if len(got.HarnessRules) != 1 || got.HarnessRules[0].PathPrefix != "/w/CL" {
		t.Fatalf("rules = %+v, want untouched by the unrelated write", got.HarnessRules)
	}
}

// The retired startupCommand/startupRules keys must stay silent no-ops: an old
// lens PUTs them on every settings save, and a 400 there would break its whole
// save. This pins decodeJSON's ignore-unknown-keys behavior — a future
// DisallowUnknownFields refactor trips here first.
func TestSettings_LegacyStartupKeysIgnored(t *testing.T) {
	s := settingsServer(t)

	rec := httptest.NewRecorder()
	s.putSettings(rec, httptest.NewRequest("PUT", "/v1/settings", strings.NewReader(
		`{"startupCommand": "claude --old", "startupRules": [{"pathPrefix": "/a", "command": "b"}]}`)))
	if rec.Code != 200 {
		t.Fatalf("legacy-keys put = %d (%s)", rec.Code, rec.Body)
	}
	var got map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if _, ok := got["startupCommand"]; ok {
		t.Fatal("GET still reports the retired startupCommand key")
	}
	if _, ok := got["startupRules"]; ok {
		t.Fatal("GET still reports the retired startupRules key")
	}
	if rules, ok := got["harnessRules"].([]any); !ok || len(rules) != 0 {
		t.Fatalf("harnessRules = %v, want present and untouched", got["harnessRules"])
	}
}
