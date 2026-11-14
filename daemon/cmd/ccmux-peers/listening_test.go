package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// lockedBuffer is a buffer the push loop can write notifications into while
// the test reads them back.
type lockedBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuffer) text() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

func (l *lockedBuffer) channelMessages() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return notificationCount(&l.b, "notifications/claude/channel")
}

// settleFor sets listenSettle for one test.
func settleFor(t *testing.T, d time.Duration) {
	t.Helper()
	was := listenSettle
	listenSettle = d
	t.Cleanup(func() { listenSettle = was })
}

func firstToolCall(t *testing.T, a *app) {
	t.Helper()
	if _, rpcErr := a.mcp.onRequest["tools/call"](json.RawMessage(`{"name":"set_summary","arguments":{"summary":"up"}}`)); rpcErr != nil {
		t.Fatalf("tools/call: %v", rpcErr)
	}
}

// heldPushApp is a session whose push channel is attached to a daemon that
// replays frames the moment it connects, as the daemon does for a session a
// message started. poll is what that daemon answers check_messages with. The
// handshake is over; nothing else has happened. Returns the app, what it wrote
// to Claude Code, and the seqs it acked.
func heldPushApp(t *testing.T, frames []map[string]any, poll string) (*app, *lockedBuffer, chan int64) {
	t.Helper()
	acks := make(chan int64, 8)
	up := websocket.Upgrader{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/peers/ws" {
			if r.URL.Path == "/v1/peers/poll" {
				fmt.Fprint(w, poll)
				return
			}
			fmt.Fprint(w, `{}`)
			return
		}
		c, err := up.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer c.Close()
		for _, f := range frames {
			_ = c.WriteJSON(f)
		}
		for {
			var f struct {
				Type string `json:"type"`
				Seq  int64  `json:"seq"`
			}
			if c.ReadJSON(&f) != nil {
				return
			}
			if f.Type == "ack" {
				acks <- f.Seq
			}
		}
	}))
	out := &lockedBuffer{}
	a := newApp()
	a.mcp = newMCPServerIO(strings.NewReader(""), out)
	a.channelMode = true
	a.daemon, a.id = newDaemonClient(srv.URL, "t"), "p1"
	a.installHandlers()
	// dropConn first: it ends the server's read, which srv.Close waits on.
	t.Cleanup(func() { a.dropConn(); srv.Close() })

	a.mcp.onNotify["notifications/initialized"](nil)
	go func() { _ = a.pushOnce() }()
	return a, out, acks
}

func wantAck(t *testing.T, acks chan int64, want int64) {
	t.Helper()
	select {
	case seq := <-acks:
		if seq != want {
			t.Fatalf("acked %d, want %d", seq, want)
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("event %d was never acked: a held push was not let go", want)
	}
}

const startedBy = "the request that started you"

// The bug this file is for. A session started by a message attaches its push
// channel with that message already waiting, so the daemon replays it at
// once: right after the handshake, which is before Claude Code has put its
// channel handler on the connection. Claude Code dropped it, this process had
// counted it as shown and acked it, and the session sat a minute before asking
// for it again. Held until the session's first tool call, it is heard.
func TestPush_AMessageWaitingAtTheStartIsHeldUntilTheSessionCanHearIt(t *testing.T) {
	settleFor(t, time.Hour)
	a, out, acks := heldPushApp(t, []map[string]any{
		{"type": "message", "seq": 41, "from_name": "ccmux", "from_id": "r1", "text": startedBy},
	}, `{"events":[]}`)

	select {
	case seq := <-acks:
		t.Fatalf("event %d was acked before the session could hear it: Claude Code drops a push this early, and an acked event never comes back", seq)
	case <-time.After(150 * time.Millisecond):
	}
	if n := out.channelMessages(); n != 0 {
		t.Fatalf("%d channel message(s) written before the session could hear them", n)
	}

	firstToolCall(t, a)
	wantAck(t, acks, 41)
	if n := out.channelMessages(); n != 1 {
		t.Fatalf("%d channel messages for one event, want exactly 1", n)
	}
}

// The gate opens once the first tool call is ANSWERED, not when it arrives.
// When that call is check_messages, its poll gets the held message too (the
// daemon has no ack for it yet) and hands it over in the tool result; the
// push, let go only then, finds it shown and acks it without saying it a
// second time. A later message the poll never saw still goes out as a push:
// the gate holds, it does not swallow.
func TestPush_AFirstCheckMessagesShowsTheHeldMessageOnce(t *testing.T) {
	settleFor(t, time.Hour)
	const later = "a second message the poll never saw"
	held := `{"type":"message","seq":41,"from_name":"ccmux","from_id":"r1","text":"` + startedBy + `"}`
	a, out, acks := heldPushApp(t, []map[string]any{
		{"type": "message", "seq": 41, "from_name": "ccmux", "from_id": "r1", "text": startedBy},
		{"type": "message", "seq": 42, "from_name": "ccmux", "from_id": "r1", "text": later},
	}, `{"events":[`+held+`]}`)

	select {
	case seq := <-acks:
		t.Fatalf("event %d was acked before any tool call", seq)
	case <-time.After(150 * time.Millisecond): // 41 has been read and is held by now
	}
	res, rpcErr := a.mcp.onRequest["tools/call"](json.RawMessage(`{"name":"check_messages","arguments":{}}`))
	if rpcErr != nil {
		t.Fatalf("tools/call: %v", rpcErr)
	}
	answer, _ := json.Marshal(res)
	if n := strings.Count(string(answer), startedBy); n != 1 {
		t.Fatalf("the first check_messages must hand the held message over, once; its answer has it %d times:\n%s", n, answer)
	}
	wantAck(t, acks, 41)
	wantAck(t, acks, 42)
	written := out.text()
	if strings.Contains(written, startedBy) {
		t.Fatalf("the held message was shown twice: in the tool result and again as a push:\n%s", written)
	}
	if n := out.channelMessages(); n != 1 || !strings.Contains(written, later) {
		t.Fatalf("the message after it must still be pushed, once; got %d channel message(s):\n%s", n, written)
	}
}

// The handshake ending is not Claude Code listening; a tool call is, since
// only a model that is up and running can make one.
func TestListening_AToolCallOpensTheGateAndTheHandshakeDoesNot(t *testing.T) {
	settleFor(t, time.Hour)
	a := newApp()
	a.installHandlers()
	open := make(chan struct{})
	go func() { a.waitClientListening(); close(open) }()

	a.mcp.onNotify["notifications/initialized"](nil)
	select {
	case <-open:
		t.Fatal("the gate opened on the handshake alone")
	case <-time.After(50 * time.Millisecond):
	}
	firstToolCall(t, a)
	select {
	case <-open:
	case <-time.After(time.Second):
		t.Fatal("the first tool call did not open the gate")
	}
	a.waitClientListening() // and it stays open: no later push waits
}

// A session a person opened and left alone calls no tool, and mail waiting
// for it must still arrive: the gate opens by itself listenSettle after the
// handshake. Counted from the handshake, not from the process start, since
// before it there is no connection to be listening on.
func TestListening_ASessionThatCallsNothingIsHeardAfterTheSettle(t *testing.T) {
	settleFor(t, 40*time.Millisecond)
	a := newApp()
	a.installHandlers()
	open := make(chan struct{})
	go func() { a.waitClientListening(); close(open) }()

	select {
	case <-open:
		t.Fatal("the gate opened before the handshake")
	case <-time.After(150 * time.Millisecond):
	}
	a.mcp.onNotify["notifications/initialized"](nil)
	select {
	case <-open:
	case <-time.After(time.Second):
		t.Fatal("the gate never opened for a session that makes no tool call")
	}
}

// A client that never sends notifications/initialized (a protocol era without
// that step) and calls no tool has only one thing to open the gate: the
// settle clock started when the wait for the handshake gives up. Until then
// it is not started, so a client still being waited for is not taken to be
// listening; after it, the pushes that the log says go "anyway" really do.
func TestListening_AClientThatNeverFinishesTheHandshakeIsHeardOnceItIsGivenUpOn(t *testing.T) {
	settleFor(t, 20*time.Millisecond)
	was := clientReadyWait
	clientReadyWait = 250 * time.Millisecond
	t.Cleanup(func() { clientReadyWait = was })
	a := newApp()
	a.installHandlers()

	gate := make(chan struct{})
	go func() { a.waitClientListening(); close(gate) }()
	pushing := make(chan struct{})
	go func() { a.waitClientReady(); a.waitClientListening(); close(pushing) }()

	select {
	case <-gate:
		t.Fatal("the gate opened while the handshake was still being waited for: nothing had started the clock")
	case <-time.After(120 * time.Millisecond): // well past listenSettle, well short of clientReadyWait
	}
	select {
	case <-pushing:
	case <-time.After(2 * time.Second):
		t.Fatal("the wait for the handshake gave up, yet the pushes stayed held for good")
	}
}

// Every real session marks the gate open many times: each later tool call
// does, and so does the settle clock after the handshake. A second mark must
// be nothing. A second close of the same channel panics, the shim recovers
// from nothing, and the session would lose its bus on its second tool call.
// The marks are also one session's own: another app's gate stays shut.
func TestListening_TheGateOpensOnceHoweverOftenItIsMarked(t *testing.T) {
	settleFor(t, 20*time.Millisecond)
	a, other := newApp(), newApp()
	a.installHandlers()
	other.installHandlers()

	a.mcp.onNotify["notifications/initialized"](nil)
	firstToolCall(t, a)
	firstToolCall(t, a)               // a later tool call marks an open gate
	time.Sleep(80 * time.Millisecond) // and the settle clock fires on it too
	a.mcp.onNotify["notifications/initialized"](nil)
	a.waitClientListening()

	shut := make(chan struct{})
	go func() { other.waitClientListening(); close(shut) }()
	select {
	case <-shut:
		t.Fatal("one session's tool calls opened another's gate")
	case <-time.After(60 * time.Millisecond):
	}
}

// An app built without the gate (most tests) is never held.
func TestListening_NoGateNoWait(t *testing.T) {
	done := make(chan struct{})
	go func() { (&app{}).waitClientListening(); close(done) }()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("an app with no gate waited")
	}
}
