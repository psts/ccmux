package api

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"

	"ccmux.dev/ccmuxd/internal/llmproxy"
	"ccmux.dev/ccmuxd/internal/meridian"
)

// fakeSidecars records what the API asked the supervisor to run.
type fakeSidecars struct {
	specs  [][]meridian.Spec
	status map[string]meridian.Status
}

func (f *fakeSidecars) Reconcile(s []meridian.Spec)        { f.specs = append(f.specs, s) }
func (f *fakeSidecars) Status() map[string]meridian.Status { return f.status }

func TestSettings_MeridianAccountDrivesSidecarsAndReportsThem(t *testing.T) {
	s := llmServer(t)
	fake := &fakeSidecars{status: map[string]meridian.Status{"m": {Running: true, PID: 42}}}
	s.SetSidecars(fake)

	rec := httptest.NewRecorder()
	s.putSettings(rec, httptest.NewRequest("PUT", "/v1/settings", strings.NewReader(`{
		"llmAccounts": [
			{"name": "ollama", "baseURL": "http://localhost:11434"},
			{"name": "m", "kind": "meridian", "apiKey": "setup-token"}
		]}`)))
	if rec.Code != 200 {
		t.Fatalf("put = %d (%s)", rec.Code, rec.Body)
	}
	if len(fake.specs) != 1 || len(fake.specs[0]) != 1 {
		t.Fatalf("expected one reconcile with one spec, got %+v", fake.specs)
	}
	if sp := fake.specs[0][0]; sp.Name != "m" || sp.Token != "setup-token" || sp.BaseURL != llmproxy.MeridianUpstream {
		t.Fatalf("spec = %+v", sp)
	}
	if strings.Contains(rec.Body.String(), "setup-token") {
		t.Fatal("settings response echoed the sidecar token")
	}
	var got struct {
		Sidecars map[string]meridian.Status `json:"llmSidecars"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if st, ok := got.Sidecars["m"]; !ok || !st.Running || st.PID != 42 {
		t.Fatalf("llmSidecars = %+v", got.Sidecars)
	}

	// Removing the account reconciles to an empty set.
	rec = httptest.NewRecorder()
	s.putSettings(rec, httptest.NewRequest("PUT", "/v1/settings", strings.NewReader(`{
		"llmAccounts": [{"name": "ollama", "baseURL": "http://localhost:11434"}]}`)))
	if rec.Code != 200 || len(fake.specs) != 2 || len(fake.specs[1]) != 0 {
		t.Fatalf("delete: code %d, reconciles %+v", rec.Code, fake.specs)
	}
}

func TestSettings_NoSidecarsKeyWithoutSupervisor(t *testing.T) {
	s := llmServer(t)
	rec := httptest.NewRecorder()
	s.getSettings(rec, httptest.NewRequest("GET", "/v1/settings", nil))
	if strings.Contains(rec.Body.String(), "llmSidecars") {
		t.Fatal("a daemon without a supervisor must not advertise llmSidecars")
	}
}

func TestKindAllowed_MeridianOnlyWhereDeclared(t *testing.T) {
	if kindAllowed(nil, llmproxy.KindMeridian) {
		t.Error("an undeclared harness (or a shell pane) must not ride a meridian sidecar")
	}
	if kindAllowed(nil, "codex") {
		t.Error("codex stays excluded by default")
	}
	if !kindAllowed(nil, "anthropic") || !kindAllowed(nil, "claude") {
		t.Error("the default still allows Claude-dialect kinds")
	}
	if !kindAllowed([]string{"meridian", "anthropic", "openai"}, llmproxy.KindMeridian) {
		t.Error("opencode's declared kinds include meridian")
	}
	if kindAllowed([]string{"anthropic", "openai", "claude"}, llmproxy.KindMeridian) {
		t.Error("the claude harness must never ride meridian")
	}
}
