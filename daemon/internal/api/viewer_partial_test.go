package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"ccmux.dev/ccmuxd/internal/peers"
)

func viewerAnswer(t *testing.T, srv *Server, token string) (int, map[string]any) {
	t.Helper()
	req := httptest.NewRequest("GET", "/v1/peers/viewer", nil)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	out := map[string]any{}
	_ = json.NewDecoder(rec.Body).Decode(&out)
	return rec.Code, out
}

// The answer has to be usable by whoever receives it. Naming the relay to a
// caller that was given no key sent the browser lens somewhere it could only be
// refused — every read 401, on every member host, permanently — while the
// handler's own contract promised it would fall back and say so.
func TestPeersViewerCredential_NoTokenIsNeverSentToTheRelay(t *testing.T) {
	_, srv := viewerCredServer(t)
	srv.SetBusResolver(func(string) (string, string, error) {
		return "http://127.0.0.1:7900/v1/hubbus", "t", nil
	})

	code, out := viewerAnswer(t, srv, "")
	if code != http.StatusOK {
		t.Fatalf("status = %d, want 200", code)
	}
	if out["token"] != "" {
		t.Error("a caller with no credential was handed the viewer token")
	}
	if out["bus"] != "" {
		t.Errorf("bus = %q for a caller that cannot read it — it must be sent to the local routes", out["bus"])
	}
	if out["partial"] != true {
		t.Error("partial must say the local routes are not the whole fleet, or the lens claims silence it cannot see")
	}
}

// With the credential, the same daemon names the relay and hands over the key.
func TestPeersViewerCredential_TokenGetsTheRelay(t *testing.T) {
	_, srv := viewerCredServer(t)
	srv.SetBusResolver(func(string) (string, string, error) {
		return "http://127.0.0.1:7900/v1/hubbus", "t", nil
	})

	code, out := viewerAnswer(t, srv, peers.PanelessToken(testSecret))
	if code != http.StatusOK {
		t.Fatalf("status = %d, want 200", code)
	}
	if out["bus"] != HubBusPrefix {
		t.Errorf("bus = %q, want %q", out["bus"], HubBusPrefix)
	}
	if out["token"] != peers.ViewerToken(testSecret) {
		t.Error("a proven caller must get the read-only token")
	}
	if out["partial"] == true {
		t.Error("a caller reading the hub is not partial")
	}
}

// A WRONG credential is refused rather than quietly given the tokenless answer.
// The Mac app clears its cached token on a 401, and a stale token is exactly the
// case that needs that re-mint; a 200 leaves it stuck presenting the old one.
func TestPeersViewerCredential_BadTokenIs401(t *testing.T) {
	_, srv := viewerCredServer(t)

	code, out := viewerAnswer(t, srv, "not-the-local-secret")
	if code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", code)
	}
	if out["token"] != nil && out["token"] != "" {
		t.Error("a refused caller was still handed a token")
	}
}
