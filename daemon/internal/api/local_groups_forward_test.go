package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"ccmux.dev/ccmuxd/internal/hub"
	"ccmux.dev/ccmuxd/internal/peers"
	"ccmux.dev/ccmuxd/internal/store"
	"ccmux.dev/ccmuxd/internal/version"
)

// The Mac app only ever talks to its own daemon, so the daemon is what carries
// its driver-mode pane map to the hub. Without the forward, a session that
// registered on the hub groups by directory and lands in a project nobody is
// looking at.
func TestPeersLocalGroups_ForwardsThisHostsMapOnward(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "peers.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	svc := peers.NewService(st, nullHook{}, testSecret)
	svc.OpenCmd = ""

	forwarded := make(chan map[string]string, 1)
	s := &Server{peersSvc: svc, upgrader: websocket.Upgrader{}}
	s.SetLocalGroupsForwarder(func(g map[string]string) { forwarded <- g })
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)

	body, _ := json.Marshal(map[string]any{"groups": map[string]string{"pane-uuid": "Window A"}})
	req, _ := http.NewRequest(http.MethodPut, ts.URL+"/v1/peers/local-groups", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+peers.PanelessToken(testSecret))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}

	select {
	case got := <-forwarded:
		if got["pane-uuid"] != "Window A" {
			t.Errorf("forwarded %v, want the pushed map", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the map was stored locally but never forwarded to the hub")
	}
}

// The credential is still required — the forward must not become a way to write
// grouping without one.
func TestPeersLocalGroups_StillNeedsTheToken(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "peers.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	svc := peers.NewService(st, nullHook{}, testSecret)
	svc.OpenCmd = ""

	// A channel, not a bool: the forward runs on its own goroutine, so a plain
	// flag read here is a data race the moment the auth check moves above it —
	// and it would surface as a -race report rather than this assertion.
	forwarded := make(chan map[string]string, 1)
	s := &Server{peersSvc: svc, upgrader: websocket.Upgrader{}}
	s.SetLocalGroupsForwarder(func(g map[string]string) { forwarded <- g })
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)

	body, _ := json.Marshal(map[string]any{"groups": map[string]string{"pane-uuid": "Window A"}})
	req, _ := http.NewRequest(http.MethodPut, ts.URL+"/v1/peers/local-groups", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", resp.StatusCode)
	}
	select {
	case <-forwarded:
		t.Error("an unauthorized push was forwarded to the hub")
	case <-time.After(200 * time.Millisecond):
	}
}

// The hub keys a member's map by the host the connection came FROM. If that
// lookup misses and the push is filed as local, the member's panes overwrite the
// hub's own slice and the hub's real panes vanish from their windows.
func TestPeersLocalGroups_MemberPushIsFiledUnderItsOwnHost(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "peers.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	svc := peers.NewService(st, nullHook{}, testSecret)
	svc.OpenCmd = ""
	svc.EnableFederation(nil, nil, func(ip string) (string, bool) {
		return "mac-one", ip == "100.0.0.1"
	})

	// A real registry, because peerConnAllowed admits a remote caller only via
	// IsMemberIP — the same map HostForConn reads. Faking one side would let the
	// test pass on a wiring that production refuses.
	reg := hub.NewRegistry("hub", hub.DefaultFloor,
		func() ([]hub.Node, error) {
			return []hub.Node{
				{ID: "hub", Addr: "hub.invalid", IPs: []string{"100.0.0.7"}},
				{ID: "mac-one", Addr: "mac-one.invalid", IPs: []string{"100.0.0.1"}},
			}, nil
		},
		func(string) (hub.Health, error) { return hub.Health{Contract: version.Contract}, nil },
		func() int64 { return 1 },
	)
	reg.Refresh()

	forwarded := make(chan map[string]string, 1)
	s := &Server{peersSvc: svc, upgrader: websocket.Upgrader{}, hub: &hubMode{reg: reg, selfID: "hub"}}
	s.SetLocalGroupsForwarder(func(g map[string]string) { forwarded <- g })
	handler := s.Handler()

	push := func(remote string) int {
		body, _ := json.Marshal(map[string]any{"groups": map[string]string{"pane-uuid": "Window A"}})
		req := httptest.NewRequest(http.MethodPut, "/v1/peers/local-groups", bytes.NewReader(body))
		req.RemoteAddr = remote
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+peers.PanelessToken(testSecret))
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		return rec.Code
	}

	if code := push("100.0.0.1:5000"); code != http.StatusOK {
		t.Fatalf("member push status = %d, want 200", code)
	}
	// A member's map is already at its destination — re-forwarding it would send
	// another host's panes upstream as this daemon's own.
	select {
	case g := <-forwarded:
		t.Errorf("a member's push was re-forwarded to the hub: %v", g)
	case <-time.After(200 * time.Millisecond):
	}

	// A non-member address never gets as far as the host lookup: peerConnAllowed
	// turns it away first. Asserted here so that ordering stays true — if the
	// origin gate were ever relaxed, this push would reach HostForConn and the
	// unresolved-host guard below becomes the only thing standing between a
	// stranger and the hub's own slice.
	if code := push("100.0.0.99:5000"); code != http.StatusForbidden {
		t.Errorf("non-member push status = %d, want 403 from the origin gate", code)
	}
}

// The guard behind that gate, tested directly because the HTTP layer cannot
// reach it: peerConnAllowed and HostForConn read the same member map, so the
// only way they disagree is a registry refresh landing between the two. On that
// miss the push must be refused rather than filed as local — filing another
// host's panes under this daemon's own label deletes the real ones.
func TestHostForConn_UnresolvedRemoteIsNotLocal(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "peers.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	svc := peers.NewService(st, nullHook{}, testSecret)
	svc.OpenCmd = ""
	svc.EnableFederation(nil, nil, func(ip string) (string, bool) {
		return "mac-one", ip == "100.0.0.1"
	})

	if host, ok := svc.HostForConn("100.0.0.1"); !ok || host != "mac-one" {
		t.Errorf("member IP → (%q, %v), want (mac-one, true)", host, ok)
	}
	if host, ok := svc.HostForConn("100.0.0.99"); ok {
		t.Errorf("unresolved remote IP → (%q, true), want ok=false — \"\" here means LOCAL", host)
	}
	if host, ok := svc.HostForConn("127.0.0.1"); !ok || host != "" {
		t.Errorf("loopback → (%q, %v), want (\"\", true) — genuinely this daemon's own", host, ok)
	}
}
