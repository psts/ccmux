package api

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"ccmux.dev/ccmuxd/internal/peers"
)

// A lens read may arrive from off-loopback — the lens page is also served on the
// daemon's tsnet listener — so the relay does not refuse it on address. It
// refuses it on credential, and the credential is what decides, not where the
// request came from.
func TestHubBusRelay_ViewerReadFromTheTailnetWithAToken(t *testing.T) {
	reached := false
	handler, _ := viewerRelayHandler(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reached = true
		writeJSON(w, http.StatusOK, []map[string]string{{"id": "peer-1"}})
	}))

	req := httptest.NewRequest("GET", HubBusPrefix+"/v1/peers?group=ChartLabs", nil)
	req.RemoteAddr = "100.80.201.127:54321"
	req.Header.Set("Authorization", "Bearer "+peers.ViewerToken(testSecret))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK || !reached {
		t.Fatalf("status = %d, reached hub = %v — a credentialled lens read must relay", rec.Code, reached)
	}
}

// The hole this closes: an unprivileged local account on a shared host reaches
// this daemon's own tsnet node through the machine's tailscaled, so its requests
// are not loopback. A relay that demanded a credential only from loopback handed
// that account the entire fleet's peer traffic.
func TestHubBusRelay_ViewerReadFromTheTailnetWithoutAToken(t *testing.T) {
	reached := false
	handler, _ := viewerRelayHandler(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reached = true
	}))

	for _, path := range []string{
		"/v1/peers?group=ChartLabs",
		"/v1/peers/messages?group=ChartLabs",
		"/v1/peers/ws?mode=listen&group=ChartLabs",
	} {
		req := httptest.NewRequest("GET", HubBusPrefix+path, nil)
		req.RemoteAddr = "100.80.201.127:54321"
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("%s: status = %d, want 401", path, rec.Code)
		}
	}
	if reached {
		t.Error("an uncredentialled read reached the hub")
	}
}

// Bus traffic is a different matter: it wears this host's identity upstream and
// can act as peers, so it stays loopback-only whatever the read surface does.
func TestHubBusRelay_BusTrafficStaysLoopbackOnly(t *testing.T) {
	reached := false
	handler, _ := viewerRelayHandler(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reached = true
	}))

	for _, path := range []string{"/v1/peers/register", "/v1/peers/send", "/v1/peers/ws?peer_id=p1"} {
		req := httptest.NewRequest("POST", HubBusPrefix+path, nil)
		req.RemoteAddr = "100.80.201.127:54321"
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		if rec.Code != http.StatusForbidden {
			t.Errorf("%s: status = %d, want 403 from a non-loopback caller", path, rec.Code)
		}
	}
	if reached {
		t.Error("bus traffic from a non-loopback caller reached the hub")
	}
}
