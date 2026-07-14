package manager

import (
	"testing"
	"time"

	"ccmux.dev/ccmuxd/internal/model"
)

func TestFirehose_PublishReachesSubscriber(t *testing.T) {
	f := newFirehose()
	id, ch := f.subscribe()
	defer f.unsubscribe(id)

	f.publish(Event{Kind: "attention", WorkspaceID: "w1", PaneID: "p1", Attention: model.AttentionNeedsInput})

	select {
	case ev := <-ch:
		if ev.Kind != "attention" || ev.WorkspaceID != "w1" || ev.PaneID != "p1" || ev.Attention != model.AttentionNeedsInput {
			t.Fatalf("unexpected event: %+v", ev)
		}
	case <-time.After(time.Second):
		t.Fatal("no event delivered")
	}
}

func TestFirehose_FanOutToAllSubscribers(t *testing.T) {
	f := newFirehose()
	id1, ch1 := f.subscribe()
	id2, ch2 := f.subscribe()
	defer f.unsubscribe(id1)
	defer f.unsubscribe(id2)

	f.publish(Event{Kind: "attention", WorkspaceID: "w"})

	for i, ch := range []<-chan Event{ch1, ch2} {
		select {
		case ev := <-ch:
			if ev.WorkspaceID != "w" {
				t.Fatalf("subscriber %d got %+v", i, ev)
			}
		case <-time.After(time.Second):
			t.Fatalf("subscriber %d received nothing", i)
		}
	}
}

// A slow subscriber must never block publishers: overflow drops, capped at the
// buffer size, and publish returns without hanging.
func TestFirehose_OverflowDropsWithoutBlocking(t *testing.T) {
	f := newFirehose()
	_, ch := f.subscribe()

	for i := 0; i < firehoseBuffer+50; i++ {
		f.publish(Event{Kind: "attention"})
	}
	if got := len(ch); got != firehoseBuffer {
		t.Fatalf("buffered %d events, want %d (excess must be dropped)", got, firehoseBuffer)
	}
}

func TestFirehose_UnsubscribeClosesAndPublishStaysSafe(t *testing.T) {
	f := newFirehose()
	id, ch := f.subscribe()
	f.unsubscribe(id)

	if _, ok := <-ch; ok {
		t.Fatal("channel should be closed after unsubscribe")
	}
	// Publishing to a hub with no live subscribers (and after a close) must not
	// panic on the closed channel.
	f.publish(Event{Kind: "attention"})
	// Unsubscribing twice is a no-op.
	f.unsubscribe(id)
}
