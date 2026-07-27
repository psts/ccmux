package api

import (
	"net/http/httptest"
	"testing"

	"ccmux.dev/ccmuxd/internal/hub"
	"ccmux.dev/ccmuxd/internal/version"
)

// memberRegistry returns a hub registry whose one member "b" has IP 100.0.0.2.
func memberRegistry(t *testing.T) *hub.Registry {
	t.Helper()
	r := hub.NewRegistry("hub", 1,
		func() ([]hub.Node, error) {
			return []hub.Node{{ID: "b", Addr: "b.ts.net", IPs: []string{"100.0.0.2"}}}, nil
		},
		func(string) (hub.Health, error) { return hub.Health{Contract: version.Contract}, nil },
		func() int64 { return 1 },
	)
	r.Refresh()
	return r
}

func allowed(t *testing.T, s *Server, remoteAddr string) bool {
	t.Helper()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/v1/peers/register", nil)
	req.RemoteAddr = remoteAddr
	return s.peerConnAllowed(rec, req)
}

// TestPeerConnAllowed_SingleHost: without a hub, the bus is loopback-only —
// exactly as before federation.
func TestPeerConnAllowed_SingleHost(t *testing.T) {
	s := &Server{} // hub == nil
	if !allowed(t, s, "127.0.0.1:5000") {
		t.Error("loopback must be allowed")
	}
	if allowed(t, s, "100.0.0.2:5000") {
		t.Error("single-host mode must reject non-loopback (loopback-only)")
	}
}

// TestPeerConnAllowed_HubMode: the hub also accepts registered member-host IPs,
// still rejecting unknown tailnet nodes.
func TestPeerConnAllowed_HubMode(t *testing.T) {
	s := &Server{hub: &hubMode{reg: memberRegistry(t), selfID: "hub"}}
	if !allowed(t, s, "127.0.0.1:5000") {
		t.Error("loopback must always be allowed")
	}
	if !allowed(t, s, "100.0.0.2:5000") {
		t.Error("a member host's IP must be allowed in hub mode")
	}
	if allowed(t, s, "100.9.9.9:5000") {
		t.Error("a non-member tailnet node must be rejected")
	}
}
