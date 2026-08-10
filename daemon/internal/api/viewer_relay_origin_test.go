package api

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// The browser lens is served on the daemon's tailnet listener, so its relay
// requests arrive from a 100.x address, not loopback. Gating the whole relay on
// loopback made the lens's bus prefix unusable: the daemon handed the page a bus
// and then refused every request to it.
func TestHubBusRelay_ViewerReadFromTheTailnet(t *testing.T) {
	reached := false
	handler, _ := viewerRelayHandler(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reached = true
		writeJSON(w, http.StatusOK, []map[string]string{{"id": "peer-1"}})
	}))

	req := httptest.NewRequest("GET", HubBusPrefix+"/v1/peers?group=ChartLabs", nil)
	req.RemoteAddr = "100.80.201.127:54321" // a tailnet peer, no credential
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK || !reached {
		t.Fatalf("status = %d, reached hub = %v — a tailnet lens read must relay", rec.Code, reached)
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
