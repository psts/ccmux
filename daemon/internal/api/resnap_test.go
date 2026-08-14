package api

import (
	"sort"
	"testing"
	"time"
)

// A resnapper must not fire until the resizes stop. A window drag emits one
// resize per grid cell, and capturing on each would run a capture-pane per cell
// of a screen the user is still dragging.
func TestResnapper_CoalescesADrag(t *testing.T) {
	rs := newResnapper()
	defer rs.stop()

	for i := 0; i < 5; i++ {
		rs.request("pane-1")
		select {
		case pane := <-rs.requests():
			rs.arm(pane)
		default:
			t.Fatal("request was not queued")
		}
		select {
		case <-rs.due():
			t.Fatal("fired while resizes were still arriving")
		case <-time.After(resizeSettle / 5):
		}
	}

	select {
	case <-rs.due():
	case <-time.After(2 * resizeSettle):
		t.Fatal("never fired after the resizes stopped")
	}
	if got := rs.take(); len(got) != 1 || got[0] != "pane-1" {
		t.Fatalf("take() = %v, want [pane-1]", got)
	}
}

// Every pane that moved is repainted, not just the last one. One window event
// re-asserts the size of every visible pane in a split, so a slot that only kept
// the newest would leave the neighbours drawn at the old width.
func TestResnapper_RepaintsEveryPaneThatMoved(t *testing.T) {
	rs := newResnapper()
	defer rs.stop()

	rs.arm("pane-a")
	rs.arm("pane-b")
	rs.arm("pane-a") // a repeat is not a second repaint
	<-rs.due()

	got := rs.take()
	sort.Strings(got)
	if len(got) != 2 || got[0] != "pane-a" || got[1] != "pane-b" {
		t.Fatalf("take() = %v, want [pane-a pane-b]", got)
	}
}

// Taking clears the set — a later timer tick must not repaint a pane nobody
// asked about.
func TestResnapper_TakeIsOneShot(t *testing.T) {
	rs := newResnapper()
	defer rs.stop()

	rs.arm("pane-a")
	<-rs.due()
	if got := rs.take(); len(got) != 1 || got[0] != "pane-a" {
		t.Fatalf("take() = %v, want [pane-a]", got)
	}
	if got := rs.take(); len(got) != 0 {
		t.Fatalf("second take() = %v, want empty", got)
	}
}

// A burst from one window event must survive the channel: with a single slot,
// every pane but one was dropped before the writer ever saw it.
func TestResnapper_QueuesABurstOfDistinctPanes(t *testing.T) {
	rs := newResnapper()
	defer rs.stop()

	panes := []string{"pane-a", "pane-b", "pane-c", "pane-d"}
	for _, p := range panes {
		rs.request(p)
	}
	for range panes {
		select {
		case pane := <-rs.requests():
			rs.arm(pane)
		default:
			t.Fatal("a pane's request was dropped before the writer saw it")
		}
	}
	<-rs.due()
	if got := rs.take(); len(got) != len(panes) {
		t.Fatalf("take() returned %d panes, want %d", len(got), len(panes))
	}
}

// request must never block the read goroutine: it shares nothing with the writer
// but this channel, and a writer busy sending output would otherwise stall input
// for the whole connection.
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
