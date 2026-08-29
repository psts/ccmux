package api

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"ccmux.dev/ccmuxd/internal/tmux"

	"github.com/gorilla/websocket"
)

// TestInputFailureNotice_OnlyForTruncation pins which failures are worth
// interrupting someone over. A send that delivered nothing already failed
// visibly — no text appeared — but a truncated one looks like it worked and
// leaves the pane holding a prefix that the next Enter runs.
func TestInputFailureNotice_OnlyForTruncation(t *testing.T) {
	partial := &tmux.PartialSendError{Pane: "%3", Sent: 4096, Total: 262144, Err: errors.New("closed")}
	got := inputFailureNotice(partial)
	if got == "" {
		t.Fatal("a truncated paste must produce a notice")
	}
	// The numbers are the content; a message without them says nothing useful.
	if !strings.Contains(got, "4096") || !strings.Contains(got, "262144") {
		t.Errorf("notice lost the byte counts: %q", got)
	}

	for name, err := range map[string]error{
		"nothing delivered": &tmux.PartialSendError{Pane: "%3", Sent: 0, Total: 100, Err: errors.New("x")},
		"untyped error":     errors.New("some other failure"),
		"no error":          nil,
	} {
		if n := inputFailureNotice(err); n != "" {
			t.Errorf("%s should not notify the user, got %q", name, n)
		}
	}

	// Wrapped errors must still be recognised — the error crosses two layers
	// before it reaches here.
	wrapped := errors.Join(errors.New("context"), partial)
	if inputFailureNotice(wrapped) == "" {
		t.Error("a wrapped PartialSendError must still produce a notice")
	}
}

// TestNoticeQueue_CoalescesPerPane pins the map-keyed-by-pane choice. A
// connection dying mid-paste fails every queued job on that pane, and fifty
// copies of the same sentence is not fifty times the information — but two
// different panes must never crowd each other out.
func TestNoticeQueue_CoalescesPerPane(t *testing.T) {
	q := newNoticeQueue()
	for i := 0; i < 50; i++ {
		q.post("%1", "first pane message")
	}
	q.post("%2", "second pane message")
	q.post("%1", "newest wins")

	q.mu.Lock()
	defer q.mu.Unlock()
	if len(q.pending) != 2 {
		t.Fatalf("pending has %d entries, want one per pane (2)", len(q.pending))
	}
	if q.pending["%1"] != "newest wins" {
		t.Errorf("pane %%1 kept %q, want the newest message", q.pending["%1"])
	}
	if q.pending["%2"] == "" {
		t.Error("a second pane's notice was crowded out by the first pane's")
	}
}

// TestNoticeQueue_PostNeverBlocks matters because post runs on a pane's sender
// goroutine, which every lens attached to that pane shares. Waiting on one slow
// socket there would stall input for all of them.
func TestNoticeQueue_PostNeverBlocks(t *testing.T) {
	q := newNoticeQueue()
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 1000; i++ { // far past the 1-slot wake channel
			q.post("%1", "msg")
		}
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("post blocked — a full wake channel must not stall a sender goroutine")
	}
}

// TestNoticeQueue_FlushWritesAndClears covers the writer half against a real
// socket, including that a drained queue does not re-send on the next wake.
func TestNoticeQueue_FlushWritesAndClears(t *testing.T) {
	q := newNoticeQueue()
	q.post("%7", "Paste was cut short: 10 of 99 bytes reached this pane.")

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := (&websocket.Upgrader{}).Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		if err := q.flush(conn); err != nil {
			t.Errorf("flush: %v", err)
		}
		_ = conn.WriteJSON(wsMsg{T: "hello"}) // sentinel: proves no second notice
	}))
	defer srv.Close()

	conn, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(srv.URL, "http"), nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	var got wsMsg
	if err := conn.ReadJSON(&got); err != nil {
		t.Fatalf("read: %v", err)
	}
	if got.T != "notice" || got.Pane != "%7" || !strings.Contains(got.Notice, "cut short") {
		t.Fatalf("frame = %+v, want a notice for %%7", got)
	}
	if err := conn.ReadJSON(&got); err != nil {
		t.Fatalf("read sentinel: %v", err)
	}
	if got.T != "hello" {
		t.Errorf("flush re-sent a drained notice: %+v", got)
	}

	q.mu.Lock()
	n := len(q.pending)
	q.mu.Unlock()
	if n != 0 {
		t.Errorf("pending still holds %d after flush", n)
	}
}
