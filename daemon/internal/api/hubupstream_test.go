package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// silentMember serves a WebSocket that accepts the connection and then does
// nothing at all: no frames, no reads (so no automatic pongs), no close. That is
// what a member host looks like when its machine sleeps or its tailnet path is
// dropped — the socket is still nominally open and nothing will ever error on it.
//
// `reads` makes the member answer pings instead, by parking in ReadMessage where
// gorilla's default ping handler replies for it.
func silentMember(t *testing.T, reads bool) *httptest.Server {
	t.Helper()
	stop := make(chan struct{})
	t.Cleanup(func() { close(stop) })

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		up := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
		conn, err := up.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		if reads {
			go func() {
				for {
					if _, _, err := conn.ReadMessage(); err != nil {
						return
					}
				}
			}()
		}
		<-stop
	}))
	t.Cleanup(srv.Close)
	return srv
}

func dialMember(t *testing.T, srv *httptest.Server) *websocket.Conn {
	t.Helper()
	conn, _, err := websocket.DefaultDialer.Dial(strings.Replace(srv.URL, "http", "ws", 1), nil)
	if err != nil {
		t.Fatalf("dial member: %v", err)
	}
	t.Cleanup(func() { conn.Close() })
	return conn
}

func testUpstreams(host string) *eventUpstreams {
	return &eventUpstreams{
		frames:    make(chan []byte, 8),
		ka:        keepalive{ping: 20 * time.Millisecond, read: 60 * time.Millisecond, pingWrite: time.Second},
		connected: map[string]bool{host: true},
	}
}

// runForward returns a channel closed when forward gives up on the host.
func runForward(u *eventUpstreams, host string, conn *websocket.Conn) chan struct{} {
	done := make(chan struct{})
	go func() {
		defer close(done)
		u.forward(context.Background(), host, conn)
	}()
	return done
}

// The regression this whole upstream keepalive exists for.
//
// forward used to block in ReadMessage with no deadline. A member that died
// without closing never produced an error, so forward never returned, so its
// deferred cleanup never ran, so `connected[host]` stayed true — and dialAll's
// "already connected" check then skipped that host on every reconnect tick for
// the life of the process. The hub silently stopped relaying that host's
// attention and stopped pushing for it, with nothing logged.
func TestEventUpstreams_ForwardReleasesASilentMember(t *testing.T) {
	u := testUpstreams("h1")
	done := runForward(u, "h1", dialMember(t, silentMember(t, false)))

	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("forward never returned: the hub would never re-dial this host again")
	}

	u.mu.Lock()
	defer u.mu.Unlock()
	if u.connected["h1"] {
		t.Error("host is still marked connected, so dialAll will skip it forever")
	}
}

// The other half: a member that is merely quiet must NOT be dropped. Members go
// hours without an attention change, and reaping those would turn the fix into a
// worse bug than the one it replaces.
//
// Note the member here never sends a frame either. It survives purely because it
// answers the hub's pings, which is what makes this work against a member running
// an older build that does not ping on its own.
func TestEventUpstreams_ForwardKeepsAQuietMember(t *testing.T) {
	u := testUpstreams("h1")
	done := runForward(u, "h1", dialMember(t, silentMember(t, true)))

	select {
	case <-done:
		t.Fatal("forward dropped a member that was answering pings")
	case <-time.After(400 * time.Millisecond): // many ping intervals and deadlines
	}
}
