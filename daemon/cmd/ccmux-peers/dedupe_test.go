package main

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

func dedupeApp(out *bytes.Buffer) *app {
	return &app{mcp: newMCPServerIO(strings.NewReader(""), out), channelMode: true}
}

func notificationCount(out *bytes.Buffer, method string) int {
	n := 0
	for _, line := range strings.Split(strings.TrimSpace(out.String()), "\n") {
		if line == "" {
			continue
		}
		var f map[string]any
		if json.Unmarshal([]byte(line), &f) == nil && f["method"] == method {
			n++
		}
	}
	return n
}

// The bug: the daemon's cursor only advances when this process acks a push, and
// it acks only after the notification write succeeds. A check_messages call in
// that window gets the event back and renders it a second time.
func TestDedupe_PolledEventAlreadyPushedIsNotRenderedTwice(t *testing.T) {
	var out bytes.Buffer
	a := dedupeApp(&out)

	if !a.dispatchEvent(wireEvent{Type: "message", Seq: 7, FromName: "backend", Text: "ship it"}) {
		t.Fatal("push dispatch failed")
	}
	if !a.alreadyShown(7) {
		t.Fatal("a pushed-and-written event must count as shown")
	}
	if got := notificationCount(&out, "notifications/claude/channel"); got != 1 {
		t.Fatalf("got %d channel notifications, want 1", got)
	}
}

// A push whose notification write FAILED must still be deliverable by polling.
// This is why the dedupe lives here and not as a skip in the daemon: the daemon
// cannot tell a delivered event from one whose write failed, so a daemon-side
// skip would swallow exactly this case.
func TestDedupe_FailedWriteIsStillPollable(t *testing.T) {
	a := &app{mcp: newMCPServerIO(strings.NewReader(""), failingWriter{}), channelMode: true}

	if a.dispatchEvent(wireEvent{Type: "message", Seq: 7, Text: "ship it"}) {
		t.Fatal("dispatch reported success despite an unwritable transport")
	}
	if a.alreadyShown(7) {
		t.Error("an event whose write failed must NOT count as shown, or polling would skip it and it would be lost")
	}
}

// A repeated push of the same seq — which happens after a reconnect, because the
// server replays from the last ACKED seq — must not reach the model twice.
func TestDedupe_ReplayAfterReconnectIsSuppressed(t *testing.T) {
	var out bytes.Buffer
	a := dedupeApp(&out)

	a.dispatchEvent(wireEvent{Type: "message", Seq: 4, Text: "first"})
	// Reconnect: the server replays 4 because the ack never landed.
	if !a.dispatchEvent(wireEvent{Type: "message", Seq: 4, Text: "first"}) {
		t.Fatal("a replayed event must still be acked away, not left to loop")
	}

	if got := notificationCount(&out, "notifications/claude/channel"); got != 1 {
		t.Errorf("got %d channel notifications for one message, want 1", got)
	}
}

// Verdicts share the dedupe, and they must not be double-emitted either: two
// verdicts for one dialog is a confusing no-op at best.
func TestDedupe_VerdictIsNotEmittedTwice(t *testing.T) {
	var out bytes.Buffer
	a := dedupeApp(&out)

	a.dispatchEvent(wireEvent{Type: "permission_verdict", Seq: 9, RequestID: "abcde", Behavior: "allow"})
	a.dispatchEvent(wireEvent{Type: "permission_verdict", Seq: 9, RequestID: "abcde", Behavior: "allow"})

	if got := notificationCount(&out, "notifications/claude/channel/permission"); got != 1 {
		t.Errorf("got %d permission notifications, want 1", got)
	}
}

// Only a forward move counts, so frames arriving out of order cannot rewind the
// high-water mark and re-open the duplicate window.
func TestMarkShown_OnlyMovesForward(t *testing.T) {
	a := &app{}

	if !a.markShown(5) {
		t.Fatal("first advance to 5 should be news")
	}
	if a.markShown(3) {
		t.Error("a lower seq must not count as news")
	}
	if !a.alreadyShown(3) || !a.alreadyShown(5) {
		t.Error("everything at or below the high-water mark is shown")
	}
	if a.alreadyShown(6) {
		t.Error("6 has not been shown")
	}
	if a.markShown(5) {
		t.Error("re-marking the same seq must not count as news")
	}
}

// An event with no seq cannot be deduped, and dropping it would lose a message.
func TestAlreadyShown_ZeroSeqIsNeverSuppressed(t *testing.T) {
	a := &app{}
	a.markShown(10)
	if a.alreadyShown(0) {
		t.Error("a zero seq must never be treated as already shown")
	}
}

// failingWriter stands in for a broken stdout so a notification write fails.
type failingWriter struct{}

func (failingWriter) Write([]byte) (int, error) { return 0, errWriteFailed }

var errWriteFailed = writeErr("stdout is closed")

type writeErr string

func (e writeErr) Error() string { return string(e) }
