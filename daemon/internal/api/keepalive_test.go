package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"ccmux.dev/ccmuxd/internal/hub"
	"ccmux.dev/ccmuxd/internal/manager"
	"ccmux.dev/ccmuxd/internal/store"
)

// firehoseServer stands up just enough daemon to serve /v1/events: the endpoint
// needs a manager to subscribe to, but no tmux and no workspaces. The keepalive
// pair is compressed to milliseconds on THIS server only — a package global
// would be read by connection goroutines that outlive the test restoring it,
// which the race detector catches.
//
// Both halves are compressed together on purpose. A deadline without a matching
// ping is precisely the broken configuration these tests exist to reject.
func firehoseServer(t *testing.T, ping, deadline time.Duration) *httptest.Server {
	t.Helper()
	st, err := store.Open(t.TempDir() + "/reg.db")
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	t.Cleanup(func() { st.Close() })

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	srv := NewServer(manager.New(ctx, nil, st))
	srv.ka.ping, srv.ka.read = ping, deadline

	hs := httptest.NewServer(srv.Handler())
	t.Cleanup(hs.Close)
	return hs
}

func dialFirehose(t *testing.T, hs *httptest.Server) *websocket.Conn {
	t.Helper()
	conn, _, err := websocket.DefaultDialer.Dial(strings.Replace(hs.URL, "http", "ws", 1)+"/v1/events", nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { conn.Close() })
	return conn
}

// An idle lens must survive indefinitely. The firehose writes nothing between
// attention changes, so without the server's ping this connection would have no
// traffic at all and the read deadline would execute it on schedule. gorilla
// answers pings automatically from inside ReadMessage, which is what keeps it up.
//
// This is the half of the pair that fails loudly if someone ever ships the read
// deadline without the ping ticker.
func TestFirehose_IdleClientSurvivesTheReadDeadline(t *testing.T) {
	conn := dialFirehose(t, firehoseServer(t, 20*time.Millisecond, 60*time.Millisecond))

	_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	if _, _, err := conn.ReadMessage(); err != nil { // the opening hello
		t.Fatalf("hello: %v", err)
	}

	// Sit in ONE long read spanning many server deadlines. It has to be a single
	// read: gorilla marks a connection permanently failed after a read timeout
	// ("repeated read on failed websocket connection"), so polling with short
	// deadlines would destroy the very thing under test. Answering a ping happens
	// inside this blocking call.
	//
	// Timing out here is success. It means the socket stayed open and silent for
	// far longer than the server's own deadline, and it doubles as proof that
	// pings never surface to the application as messages — every other firehose
	// test reads frames positionally and would break if they did.
	_ = conn.SetReadDeadline(time.Now().Add(400 * time.Millisecond))
	_, _, err := conn.ReadMessage()
	if err == nil {
		t.Fatal("a ping was delivered to the application as a message")
	}
	if !isTimeout(err) {
		t.Fatalf("idle client was dropped while it was still answering pings: %v", err)
	}
}

// A lens whose network died without a close handshake must be reaped. This is the
// case that used to hang forever: the daemon never writes to an idle firehose, so
// nothing manufactured an error and the connection (plus its presence entry) sat
// there until the process restarted.
//
// Suppressing the client's ping handler reproduces a peer that is gone but whose
// socket is still nominally open, which no amount of waiting would otherwise
// reveal.
func TestFirehose_SilentClientIsReaped(t *testing.T) {
	conn := dialFirehose(t, firehoseServer(t, 20*time.Millisecond, 60*time.Millisecond))

	// Never pong. The default handler would answer for us.
	conn.SetPingHandler(func(string) error { return nil })

	// The client deadline is generous and must NOT be what ends this read. A bare
	// "any error means success" here passes even with reaping switched off
	// entirely, because the client's own timeout counts as an error — so insist on
	// a real hangup.
	_ = conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	for {
		_, _, err := conn.ReadMessage()
		if err == nil {
			continue
		}
		if isTimeout(err) {
			t.Fatal("client gave up before the daemon reaped it: the silent peer was never dropped")
		}
		return // the daemon hung up, which is the whole point
	}
}

// The hello frame still arrives before any of the keepalive machinery runs, so a
// lens joining a quiet daemon is not left guessing.
func TestFirehose_HelloPrecedesKeepalive(t *testing.T) {
	conn := dialFirehose(t, firehoseServer(t, 20*time.Millisecond, 60*time.Millisecond))

	_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	var frame firehoseMsg
	if err := conn.ReadJSON(&frame); err != nil {
		t.Fatalf("hello: %v", err)
	}
	if frame.T != "hello" {
		t.Errorf("first frame is %q, want hello", frame.T)
	}
}

func isTimeout(err error) bool {
	type timeouter interface{ Timeout() bool }
	te, ok := err.(timeouter)
	return ok && te.Timeout()
}

// Guard the pairing itself. Someone tuning these numbers later must not leave the
// deadline shorter than the ping interval, which would reap every healthy client.
func TestKeepalive_DeadlineLeavesRoomForLostPings(t *testing.T) {
	ka := defaultKeepalive()
	if ka.read <= ka.ping {
		t.Fatalf("read deadline %v must exceed the ping interval %v or every client is reaped", ka.read, ka.ping)
	}
	if ka.read < 2*ka.ping {
		t.Errorf("read deadline %v leaves no room for a single lost ping (interval %v)", ka.read, ka.ping)
	}
}

// The attach endpoint answers 409 for an unknown workspace before any upgrade, so
// the keepalive path is unreachable there. Pinned so a future refactor that moves
// the upgrade earlier does not silently start pinging dead workspaces.
func TestAttach_UnknownWorkspaceNeverUpgrades(t *testing.T) {
	hs := firehoseServer(t, 20*time.Millisecond, 60*time.Millisecond)
	resp, err := http.Get(hs.URL + "/v1/attach?workspace=nope")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusConflict {
		t.Errorf("status = %d, want 409", resp.StatusCode)
	}
}

// The hub firehose is a SEPARATE handler with its own write loop, and it is the
// one every lens actually gets whenever a hub is configured (events() delegates
// to it at the top). It shipped the read deadline without a ping ticker, so every
// idle lens in the federation was reaped on a 90s timer and re-dialled every
// member host on each reconnect. The plain-firehose test could not see it.
func TestHubFirehose_IdleClientSurvivesTheReadDeadline(t *testing.T) {
	st, err := store.Open(t.TempDir() + "/reg.db")
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	srv := NewServer(manager.New(ctx, nil, st))
	srv.ka.ping, srv.ka.read = 20*time.Millisecond, 60*time.Millisecond
	// A hub with no reachable members: enough to route /v1/events into hubEvents,
	// which is the code path under test.
	reg := hub.NewRegistry("hub", hub.DefaultFloor,
		func() ([]hub.Node, error) { return []hub.Node{{ID: "hub", Addr: "hub.invalid"}}, nil },
		func(string) (hub.Health, error) { return hub.Health{}, nil },
		func() int64 { return 1 },
	)
	srv.hub = &hubMode{reg: reg, selfID: "hub"}

	hs := httptest.NewServer(srv.Handler())
	t.Cleanup(hs.Close)
	conn := dialFirehose(t, hs)

	_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	if _, _, err := conn.ReadMessage(); err != nil { // the opening hello
		t.Fatalf("hello: %v", err)
	}

	_ = conn.SetReadDeadline(time.Now().Add(400 * time.Millisecond))
	_, _, err = conn.ReadMessage()
	if err == nil {
		t.Fatal("a ping was delivered to the application as a message")
	}
	if !isTimeout(err) {
		t.Fatalf("idle hub lens was dropped while it was still answering pings: %v", err)
	}
}
