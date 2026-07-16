package api

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
)

// putHostnamesRec drives the handler directly (no tmux needed for the
// validation and unknown-workspace paths).
func putHostnamesRec(t *testing.T, s *Server, wsID, body string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("PUT", "/v1/workspaces/"+wsID+"/hostnames", strings.NewReader(body))
	req.SetPathValue("id", wsID)
	s.putHostnames(rec, req)
	return rec
}

func TestHostnames_Validation(t *testing.T) {
	s := settingsServer(t)

	cases := []struct {
		name string
		body string
		code int
	}{
		{"unknown workspace valid body", `{"hostnames":[{"name":"app","port":3001}]}`, 404},
		{"bad label uppercase-only symbols", `{"hostnames":[{"name":"app_x","port":3001}]}`, 400},
		{"bad label leading hyphen", `{"hostnames":[{"name":"-app","port":3001}]}`, 400},
		{"bad label empty", `{"hostnames":[{"name":"","port":3001}]}`, 400},
		{"bad port zero", `{"hostnames":[{"name":"app","port":0}]}`, 400},
		{"bad port range", `{"hostnames":[{"name":"app","port":70000}]}`, 400},
		{"dup in request", `{"hostnames":[{"name":"app","port":1},{"name":"APP","port":2}]}`, 400},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if rec := putHostnamesRec(t, s, "nope", c.body); rec.Code != c.code {
				t.Fatalf("code = %d (%s), want %d", rec.Code, rec.Body, c.code)
			}
		})
	}
}

func TestSettings_DevKeys(t *testing.T) {
	s := settingsServer(t)

	type settingsResp struct {
		DevDomain           string `json:"devDomain"`
		CloudflareTokenSet  bool   `json:"cloudflareTokenSet"`
		TailscaleAuthKeySet bool   `json:"tailscaleAuthKeySet"`
		DevCertStatus       string `json:"devCertStatus"`
	}
	put := func(body string) (int, settingsResp, string) {
		rec := httptest.NewRecorder()
		s.putSettings(rec, httptest.NewRequest("PUT", "/v1/settings", strings.NewReader(body)))
		var got settingsResp
		_ = json.Unmarshal(rec.Body.Bytes(), &got)
		return rec.Code, got, rec.Body.String()
	}

	// Domain without a token is rejected before persisting.
	if code, _, _ := put(`{"devDomain":"dev.sanlabs.io"}`); code != 400 {
		t.Fatalf("domain without token = %d, want 400", code)
	}
	if s.mgr.DevDomain() != "" {
		t.Fatal("rejected PUT persisted the domain")
	}

	// Domain + token together: accepted; GET reports presence, never the secret.
	code, got, raw := put(`{"devDomain":"Dev.SanLabs.io","cloudflareToken":"cf-secret-123"}`)
	if code != 200 {
		t.Fatalf("put = %d (%s)", code, raw)
	}
	if got.DevDomain != "dev.sanlabs.io" || !got.CloudflareTokenSet {
		t.Fatalf("resp = %+v, want lowercased domain + tokenSet", got)
	}
	if strings.Contains(raw, "cf-secret-123") {
		t.Fatalf("secret echoed in response: %s", raw)
	}
	if got.DevCertStatus != "unknown" {
		t.Fatalf("certStatus = %q, want unknown (no devhost wired)", got.DevCertStatus)
	}

	// Auth key: presence flag only.
	if _, got, raw = put(`{"tailscaleAuthKey":"tskey-abc"}`); !got.TailscaleAuthKeySet || strings.Contains(raw, "tskey-abc") {
		t.Fatalf("auth key handling wrong: %+v / %s", got, raw)
	}

	// Clearing the domain first, then the token, is allowed; status returns to unset.
	if code, got, _ = put(`{"devDomain":""}`); code != 200 || got.DevDomain != "" || got.DevCertStatus != "unset" {
		t.Fatalf("clear domain = %d %+v", code, got)
	}
	if code, got, _ = put(`{"cloudflareToken":""}`); code != 200 || got.CloudflareTokenSet {
		t.Fatalf("clear token = %d %+v", code, got)
	}
	// But clearing the token while a domain is set is rejected.
	if code, _, _ = put(`{"devDomain":"dev.sanlabs.io","cloudflareToken":"x"}`); code != 200 {
		t.Fatal("setup put failed")
	}
	if code, _, _ = put(`{"cloudflareToken":""}`); code != 400 {
		t.Fatalf("clearing token under a set domain = %d, want 400", code)
	}
}

// Happy-path + cross-workspace uniqueness live in internal/manager's
// devhost_test.go — populating a manager with workspaces needs the package-
// internal adopt() path (no tmux). The handler's error mapping and response
// shape are pinned above.
