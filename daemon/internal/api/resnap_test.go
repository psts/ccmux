package api

import (
	"sort"
	"testing"
	"time"
)

// drain reports the panes the writer would repaint right now, without a socket.
func drain(r *resnapper) []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	panes := make([]string, 0, len(r.pending))
	for pane := range r.pending {
		panes = append(panes, pane)
	}
	r.pending = map[string]struct{}{}
	sort.Strings(panes)
	return panes
}

// awaitDue fails the test rather than hanging forever if the settle window never
// closes — a regression in arm() should report itself, not stall the package.
func awaitDue(t *testing.T, r *resnapper) {
	t.Helper()
	select {
	case <-r.due():
	case <-time.After(2 * time.Second):
		t.Fatal("settle window never closed")
	}
}

// A resnapper must not fire until the resizes stop. A window drag emits one
// resize per grid cell, and capturing on each would run a capture-pane per cell
// of a screen the user is still dragging.
func TestResnapper_CoalescesADrag(t *testing.T) {
	rs := newResnapper()
	defer rs.stop()

	for i := 0; i < 5; i++ {
		rs.request("pane-1")
		<-rs.wakeups()
		rs.arm()
		select {
		case <-rs.due():
			t.Fatal("fired while resizes were still arriving")
		case <-time.After(resizeSettle / 5):
		}
	}

	awaitDue(t, rs)
	if got := drain(rs); len(got) != 1 || got[0] != "pane-1" {
		t.Fatalf("pending = %v, want [pane-1]", got)
	}
}

// Every pane that moved is repainted, not just the last one. One window event
// re-asserts the size of every visible pane in a split, so a design that kept
// only the newest would leave the neighbours drawn at the old width.
func TestResnapper_RepaintsEveryPaneThatMoved(t *testing.T) {
	rs := newResnapper()
	defer rs.stop()

	rs.request("pane-a")
	rs.request("pane-b")
	rs.request("pane-a") // a repeat is not a second repaint
	rs.arm()
	awaitDue(t, rs)

	got := drain(rs)
	if len(got) != 2 || got[0] != "pane-a" || got[1] != "pane-b" {
		t.Fatalf("pending = %v, want [pane-a pane-b]", got)
	}
}

// Flushing clears the set — a later timer tick must not repaint a pane nobody
// asked about.
func TestResnapper_FlushIsOneShot(t *testing.T) {
	rs := newResnapper()
	defer rs.stop()

	rs.request("pane-a")
	rs.arm()
	awaitDue(t, rs)
	if got := drain(rs); len(got) != 1 {
		t.Fatalf("pending = %v, want one pane", got)
	}
	if got := drain(rs); len(got) != 0 {
		t.Fatalf("second drain = %v, want empty", got)
	}
}

// The wake-up channel holds one slot, so a burst larger than it must still keep
// every distinct pane. The previous design buffered pane ids in the channel:
// once it filled with repeats of one pane, another pane's only request was
// dropped and that pane never repainted. This is that case.
func TestResnapper_KeepsEveryPaneWhenNobodyIsDraining(t *testing.T) {
	rs := newResnapper()
	defer rs.stop()

	// Far more requests than any buffer, and the writer never wakes to consume.
	for i := 0; i < 200; i++ {
		rs.request("noisy-pane")
	}
	rs.request("quiet-pane")

	got := drain(rs)
	if len(got) != 2 || got[0] != "noisy-pane" || got[1] != "quiet-pane" {
		t.Fatalf("pending = %v, want both panes kept", got)
	}
}

// request must never block the read goroutine: it would stall input for the whole
// connection while the writer is busy sending output.
func TestResnapper_RequestNeverBlocks(t *testing.T) {
	rs := newResnapper()
	defer rs.stop()

	done := make(chan struct{})
	go func() {
		for i := 0; i < 1000; i++ {
			rs.request("pane-1")
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("request blocked with nobody draining")
	}
}

// A fresh resnapper is idle. Without draining the constructor's timer, the first
// arm() would fire instantly on a stale tick and capture mid-drag.
func TestResnapper_IdleUntilArmed(t *testing.T) {
	rs := newResnapper()
	defer rs.stop()

	select {
	case <-rs.due():
		t.Fatal("fired without being armed")
	case <-time.After(2 * resizeSettle):
	}
}
