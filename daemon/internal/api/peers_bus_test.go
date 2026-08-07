package api

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"

	"ccmux.dev/ccmuxd/internal/peers"
)

// busAsk posts a bus-resolution request as pane with the given bearer token.
func busAsk(t *testing.T, s *Server, pane, token string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest("POST", "/v1/peers/bus", strings.NewReader(`{"pane_id":"`+pane+`"}`))
	req.RemoteAddr = "127.0.0.1:5000"
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rec := httptest.NewRecorder()
	s.peersBus(rec, req)
	return rec
}

func busServer(t *testing.T) (*Server, *peers.Service) {
	t.Helper()
	_, svc := newPeersTestServer(t)
	return &Server{peersSvc: svc}, svc
}

// TestPeersBus_NoHub: with no resolver wired (single-host, or a hub answering
// about itself) the empty answer means "stay where you are". The caller already
// holds that URL, so sending one would be a second source of truth.
func TestPeersBus_NoHub(t *testing.T) {
	s, _ := busServer(t)
	rec := busAsk(t, s, "pane-1", peers.TokenForPane(testSecret, "pane-1"))
	if rec.Code != 200 {
		t.Fatalf("status = %d, want 200 (%s)", rec.Code, rec.Body)
	}
	var got map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got["url"] != "" || got["token"] != "" {
		t.Errorf("answer = %v, want both empty", got)
	}
}

// TestPeersBus_Hub: the resolver's answer is passed through verbatim, per pane —
// this is the live tag:ccmux-hub lookup replacing what used to be frozen into
// pane environment at session-creation time.
func TestPeersBus_Hub(t *testing.T) {
	s, _ := busServer(t)
	var askedFor string
	s.SetBusResolver(func(paneID string) (string, string) {
		askedFor = paneID
		return "https://hub.ts.net", "hub-token-for-" + paneID
	})
	rec := busAsk(t, s, "pane-7", peers.TokenForPane(testSecret, "pane-7"))
	if rec.Code != 200 {
		t.Fatalf("status = %d, want 200 (%s)", rec.Code, rec.Body)
	}
	var got map[string]string
	_ = json.Unmarshal(rec.Body.Bytes(), &got)
	if got["url"] != "https://hub.ts.net" || got["token"] != "hub-token-for-pane-7" {
		t.Errorf("answer = %v, want the resolver's hub + minted token", got)
	}
	if askedFor != "pane-7" {
		t.Errorf("resolver asked about %q, want pane-7 — the token is per-pane", askedFor)
	}
}

// TestPeersBus_Auth: the route hands out a credential for another bus, so it
// must prove the caller owns the pane it names. A wrong or missing token, or a
// token belonging to a DIFFERENT pane, gets nothing.
func TestPeersBus_Auth(t *testing.T) {
	cases := []struct {
		name  string
		pane  string
		token string
		want  int
	}{
		{"valid pane token", "pane-1", peers.TokenForPane(testSecret, "pane-1"), 200},
		{"no token", "pane-1", "", 401},
		{"wrong token", "pane-1", "nope", 401},
		{"another pane's token", "pane-1", peers.TokenForPane(testSecret, "pane-2"), 401},
		{"no pane id", "", peers.TokenForPane(testSecret, "pane-1"), 401},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s, _ := busServer(t)
			s.SetBusResolver(func(string) (string, string) { return "https://hub.ts.net", "tok" })
			if rec := busAsk(t, s, tc.pane, tc.token); rec.Code != tc.want {
				t.Errorf("status = %d, want %d (%s)", rec.Code, tc.want, rec.Body)
			}
		})
	}
}

// TestPeersBus_NonLoopbackRefused: a pane asks its OWN daemon, so this route has
// no reason to answer anyone else. Unlike /v1/peers/pane-token, which the hub
// exposes to member hosts, this one stays loopback-only.
func TestPeersBus_NonLoopbackRefused(t *testing.T) {
	s, _ := busServer(t)
	req := httptest.NewRequest("POST", "/v1/peers/bus", strings.NewReader(`{"pane_id":"pane-1"}`))
	req.RemoteAddr = "100.0.0.2:5000"
	req.Header.Set("Authorization", "Bearer "+peers.TokenForPane(testSecret, "pane-1"))
	rec := httptest.NewRecorder()
	s.peersBus(rec, req)
	if rec.Code != 403 {
		t.Errorf("status = %d, want 403 — the bus route is loopback-only", rec.Code)
	}
}
