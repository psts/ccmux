package session

import (
	"strings"
	"sync/atomic"
)

// subBufferSize bounds a subscriber's pending-output queue. On overflow we drop
// and flag the subscriber lagged; the consumer re-seeds from a fresh Capture
// (the correct screen state) rather than replaying a corrupted byte stream.
const subBufferSize = 512

type subscriber struct {
	ch     chan Event
	lagged atomic.Bool
}

// Sub is a consumer handle for one lens attachment.
type Sub struct {
	ID int
	C  <-chan Event

	ctrl  *Controller
	inner *subscriber
}

// Lagged reports (and clears) whether output was dropped since the last check.
// When true, the consumer should Capture the affected panes and resend a
// snapshot before continuing to stream.
func (s *Sub) Lagged() bool { return s.inner.lagged.Swap(false) }

// Close unsubscribes.
func (s *Sub) Close() {
	s.ctrl.mu.Lock()
	delete(s.ctrl.subs, s.ID)
	s.ctrl.mu.Unlock()
}

// Subscribe registers a new consumer of this session's events.
func (c *Controller) Subscribe() *Sub {
	inner := &subscriber{ch: make(chan Event, subBufferSize)}
	c.mu.Lock()
	id := c.nextSub
	c.nextSub++
	c.subs[id] = inner
	c.mu.Unlock()
	return &Sub{ID: id, C: inner.ch, ctrl: c, inner: inner}
}

// Broadcast fans a manager-originated event (e.g. attention) out to every
// attached lens. Non-blocking, like output delivery.
func (c *Controller) Broadcast(ev Event) { c.fanout(ev) }

// fanout delivers an event to all subscribers without blocking the caller; a
// full subscriber is flagged lagged and re-seeds from a fresh snapshot.
func (c *Controller) fanout(ev Event) {
	c.mu.RLock()
	for _, s := range c.subs {
		select {
		case s.ch <- ev:
		default:
			s.lagged.Store(true)
		}
	}
	c.mu.RUnlock()
}

// OnOutput implements tmux.Handler. It runs on the control reader goroutine, so
// it must never block.
func (c *Controller) OnOutput(tmuxPane string, data []byte) {
	c.mu.RLock()
	ref := c.byTmuxPane[tmuxPane]
	c.mu.RUnlock()
	if ref == nil {
		return // output for an unregistered window; the attach snapshot covers it
	}
	// Copy: the caller reuses the underlying buffer.
	buf := make([]byte, len(data))
	copy(buf, data)
	c.fanout(Event{Kind: "output", PaneID: ref.id, Data: buf})
}

// OnNotification implements tmux.Handler for non-output events.
func (c *Controller) OnNotification(kind, rest string) {
	switch kind {
	case "window-close", "unlinked-window-close":
		c.handleWindowClose(strings.TrimSpace(rest))
	case "exit":
		c.emit(Notice{Kind: "exit"})
	}
}

func (c *Controller) handleWindowClose(window string) {
	c.mu.Lock()
	var paneID string
	for pane, ref := range c.byTmuxPane {
		if ref.window == window {
			paneID = ref.id
			delete(c.byTmuxPane, pane)
			delete(c.byID, ref.id)
			break
		}
	}
	c.mu.Unlock()
	c.emit(Notice{Kind: "window-close", Window: window, PaneID: paneID})
}

// emit delivers a notice without blocking the reader goroutine.
func (c *Controller) emit(n Notice) {
	select {
	case c.notices <- n:
	default:
	}
}
