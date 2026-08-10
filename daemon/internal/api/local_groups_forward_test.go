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

	"ccmux.dev/ccmuxd/internal/peers"
	"ccmux.dev/ccmuxd/internal/store"
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

	forwarded := false
	s := &Server{peersSvc: svc, upgrader: websocket.Upgrader{}}
	s.SetLocalGroupsForwarder(func(map[string]string) { forwarded = true })
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
	if forwarded {
		t.Error("an unauthorized push was forwarded to the hub")
	}
}
