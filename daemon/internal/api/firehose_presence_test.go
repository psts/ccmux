package api

import (
	"context"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"ccmux.dev/ccmuxd/internal/manager"
	"ccmux.dev/ccmuxd/internal/store"
)

// presenceServer stands up a daemon whose *Server is reachable, so a test can
// dial /v1/events and then interrogate ActiveOwners directly.
func presenceServer(t *testing.T) (*Server, *httptest.Server) {
	t.Helper()
	st, err := store.Open(t.TempDir() + "/reg.db")
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	t.Cleanup(func() { st.Close() })

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	srv := NewServer(manager.New(ctx, nil, st))
	hs := httptest.NewServer(srv.Handler())
	t.Cleanup(hs.Close)
	return srv, hs
}

// waitForOwner polls ActiveOwners until login's membership matches want; the
// focus frame is applied by the connection's read goroutine, so the test can
// only converge on it.
func waitForOwner(t *testing.T, srv *Server, login string, want bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if srv.presence.ActiveOwners()[login] == want {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("ActiveOwners[%q] never became %v (owners = %v)", login, want, srv.presence.ActiveOwners())
}

// A lens whose only daemon connection is the firehose must still be able to
// prove a person is at its screen. The Mac app holds no attach socket when no
// hosted workspace is open, and presence used to ride only on attach sockets —
// so a dev sitting at that Mac looked absent and their phone buzzed anyway.
func TestFirehose_PresenceWithoutAnAttach(t *testing.T) {
	srv, hs := presenceServer(t)
	conn, _, err := websocket.DefaultDialer.Dial(
		strings.Replace(hs.URL, "http", "ws", 1)+"/v1/events?user=dev@example.com", nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { conn.Close() })
	if _, _, err := conn.ReadMessage(); err != nil { // the opening hello
		t.Fatalf("hello: %v", err)
	}

	// Joining alone is not being at a screen: the entry must stay silent until
	// the lens says so, exactly like an attach lens that never reports.
	if srv.presence.ActiveOwners()["dev@example.com"] {
		t.Fatal("a firehose join counted as at-a-screen before any presence report")
	}

	// A non-focus frame stays a no-op — the firehose still carries no commands.
	if err := conn.WriteMessage(websocket.TextMessage, []byte(`{"t":"input","data":"x"}`)); err != nil {
		t.Fatalf("write: %v", err)
	}

	if err := conn.WriteJSON(map[string]any{"t": "focus", "present": true}); err != nil {
		t.Fatalf("write: %v", err)
	}
	waitForOwner(t, srv, "dev@example.com", true)

	if err := conn.WriteJSON(map[string]any{"t": "focus", "present": false}); err != nil {
		t.Fatalf("write: %v", err)
	}
	waitForOwner(t, srv, "dev@example.com", false)
}

// Closing the firehose must remove its presence entry, or a quit app would
// suppress its person's phone pushes forever.
func TestFirehose_PresenceClearedOnDisconnect(t *testing.T) {
	srv, hs := presenceServer(t)
	conn, _, err := websocket.DefaultDialer.Dial(
		strings.Replace(hs.URL, "http", "ws", 1)+"/v1/events?user=dev@example.com", nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	if _, _, err := conn.ReadMessage(); err != nil {
		t.Fatalf("hello: %v", err)
	}
	if err := conn.WriteJSON(map[string]any{"t": "focus", "present": true}); err != nil {
		t.Fatalf("write: %v", err)
	}
	waitForOwner(t, srv, "dev@example.com", true)

	conn.Close()
	waitForOwner(t, srv, "dev@example.com", false)
}
