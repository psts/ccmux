package session

import (
	"testing"

	"ccmux.dev/ccmuxd/internal/model"
)

// newTestController builds a Controller with only the fan-out machinery wired up
// (no tmux client). fanout/Subscribe/Drain touch just the subscriber map, so
// this is enough to unit-test the lag/reseed contract deterministically.
func newTestController() *Controller {
	return &Controller{
		byTmuxPane: map[string]*paneRef{},
		byID:       map[string]*paneRef{},
		subs:       map[int]*subscriber{},
		notices:    make(chan Notice, 8),
	}
}

// TestSub_Overflow_SetsLagged proves the drop-and-flag contract: once the buffer
// is full, further fan-out is dropped (not blocked) and the subscriber is flagged
// lagged so the consumer knows to reseed.
func TestSub_Overflow_SetsLagged(t *testing.T) {
	c := newTestController()
	sub := c.Subscribe()

	for i := 0; i < subBufferSize+50; i++ {
		c.fanout(Event{Kind: "output", PaneID: "p", Data: []byte("x")})
	}
	if !sub.inner.lagged.Load() {
		t.Fatal("expected subscriber flagged lagged after overflow")
	}
	if got := len(sub.inner.ch); got != subBufferSize {
		t.Fatalf("buffered = %d, want cap %d (excess dropped, not blocked)", got, subBufferSize)
	}
}

// TestSub_Drain_DiscardsStaleOutputKeepsControl is the regression guard for the
// reseed-after-lag corruption: on the lag path the queued OUTPUT is stale (about
// to be replaced by a fresh capture) and must be discarded, while control events
// (attention/presence/layout/pane lifecycle) are not reseed-recoverable and must
// survive the drain in order.
func TestSub_Drain_DiscardsStaleOutputKeepsControl(t *testing.T) {
	c := newTestController()
	sub := c.Subscribe()

	// Interleave two control events among a flood of stale output that overflows
	// the buffer (so a real drop + lag occurs).
	c.fanout(Event{Kind: "attention", PaneID: "p1", Attention: model.AttentionNeedsInput})
	for i := 0; i < subBufferSize; i++ {
		c.fanout(Event{Kind: "output", PaneID: "p1", Data: []byte("stale")})
	}
	c.fanout(Event{Kind: "presence", Payload: "clients"}) // dropped or kept — must not be lost if buffered

	if !sub.inner.lagged.Load() {
		t.Fatal("expected lagged after overflow")
	}

	kept := sub.Drain()

	if n := len(sub.inner.ch); n != 0 {
		t.Fatalf("channel not fully drained: %d events remain", n)
	}
	// Every kept event must be a control event; no stale output may survive.
	for _, ev := range kept {
		if ev.Kind == "output" {
			t.Fatalf("Drain returned a stale output event: %+v", ev)
		}
	}
	// The attention event was buffered first (before the flood), so it must be
	// preserved — losing an attention change to an output flood is the drift we
	// are guarding against.
	sawAttention := false
	for _, ev := range kept {
		if ev.Kind == "attention" && ev.PaneID == "p1" {
			sawAttention = true
		}
	}
	if !sawAttention {
		t.Fatalf("attention event lost across drain; kept = %+v", kept)
	}
}

// TestSub_Drain_EmptyWhenNoBacklog: draining a caught-up subscriber returns
// nothing and does not block.
func TestSub_Drain_EmptyWhenNoBacklog(t *testing.T) {
	c := newTestController()
	sub := c.Subscribe()
	if kept := sub.Drain(); len(kept) != 0 {
		t.Fatalf("Drain of empty sub returned %d events", len(kept))
	}
}
