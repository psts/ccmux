package api

import (
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
	if got := rs.take(); got != "pane-1" {
		t.Fatalf("take() = %q, want pane-1", got)
	}
}

// The last pane to move is the one repainted, and taking it clears it — a second
// timer tick must not repaint a pane nobody asked about.
func TestResnapper_TakeIsOneShot(t *testing.T) {
	rs := newResnapper()
	defer rs.stop()

	rs.arm("pane-a")
	rs.arm("pane-b")
	<-rs.due()
	if got := rs.take(); got != "pane-b" {
		t.Fatalf("take() = %q, want pane-b", got)
	}
	if got := rs.take(); got != "" {
		t.Fatalf("second take() = %q, want empty", got)
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
