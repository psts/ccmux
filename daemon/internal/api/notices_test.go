package api

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"ccmux.dev/ccmuxd/internal/session"
	"ccmux.dev/ccmuxd/internal/tmux"

	"github.com/gorilla/websocket"
)

// TestInputFailureNotice_DistinguishesTruncatedFromTotal pins the three-way
// split, and specifically that a TOTAL failure is not silent.
//
// It was silent once, on the argument that delivering nothing "fails visibly
// because no text appears". That is false wherever the pane does not echo — a
// password prompt, an ssh passphrase, sudo, `read -s` — where nothing appears
// on success either, so the user cannot tell the two apart and blames the
// credential. Sent == 0 also means the first chunk failed, which usually means
// every later send to that pane will fail too.
func TestInputFailureNotice_DistinguishesTruncatedFromTotal(t *testing.T) {
	truncated := inputFailureNotice(&tmux.PartialSendError{
		Pane: "%3", Sent: 4096, Total: 262144, Err: errors.New("closed"),
	})
	if !strings.Contains(truncated, "4096") || !strings.Contains(truncated, "262144") {
		t.Errorf("a truncated paste must quote its byte counts: %q", truncated)
	}

	total := inputFailureNotice(&tmux.PartialSendError{
		Pane: "%3", Sent: 0, Total: 100, Err: errors.New("closed"),
	})
	if total == "" {
		t.Fatal("a send that delivered NOTHING must still notify — at a password " +
			"prompt it is indistinguishable from success")
	}
	if total == truncated {
		t.Error("total and partial failure need different words: they call for " +
			"different actions from the user")
	}

	// An error of another kind still deserves a line rather than silence.
	if generic := inputFailureNotice(errors.New("some other failure")); generic == "" {
		t.Error("an untyped send failure must not be silent")
	}

	// Success is the only silent case.
	if n := inputFailureNotice(nil); n != "" {
		t.Errorf("a successful send must say nothing, got %q", n)
	}

	// Wrapped errors must still be recognised — the error crosses two layers
	// before it reaches here.
	wrapped := errors.Join(errors.New("context"), &tmux.PartialSendError{
		Pane: "%3", Sent: 7, Total: 9, Err: errors.New("x"),
	})
	if !strings.Contains(inputFailureNotice(wrapped), "7") {
		t.Error("a wrapped PartialSendError must still yield its byte counts")
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

// TestPaneWriter_DeliversQueuedNotice wires the last link: post -> writer arm ->
// socket. flush is tested directly above, but nothing exercised it THROUGH
// paneWriter.step, so deleting `case <-w.nq.wakeups():` from the select — or
// dropping nq from the paneWriter literal in the attach handler — left notices
// piling up in a map nobody drains, with every test still green.
func TestPaneWriter_DeliversQueuedNotice(t *testing.T) {
	q := newNoticeQueue()
	q.post("%4", "Paste was cut short: 8 of 64 bytes reached this pane.")

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := (&websocket.Upgrader{}).Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		// A real Sub is not needed: step selects, and only the notice arm is
		// ready. rs must be non-nil because its channels are read in the same
		// select, but it is parked and never fires here.
		pw := &paneWriter{conn: conn, nq: q, rs: newResnapper(), sub: &session.Sub{}}
		defer pw.rs.stop()
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if done := pw.step(ctx, nil); done {
			t.Error("step reported done on a notice wake-up")
		}
	}))
	defer srv.Close()

	conn, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(srv.URL, "http"), nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	var msg wsMsg
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	if err := conn.ReadJSON(&msg); err != nil {
		t.Fatalf("no frame reached the socket — the writer's notice arm is not wired: %v", err)
	}
	if msg.T != "notice" || msg.Pane != "%4" || !strings.Contains(msg.Notice, "cut short") {
		t.Errorf("frame = %+v, want a notice for %%4", msg)
	}
}
