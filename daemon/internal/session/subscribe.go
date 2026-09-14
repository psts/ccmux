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

// Drain removes every event currently queued for this subscriber, discarding
// "output" events and returning the rest in FIFO order. It is called on the lag
// path: once a drop has occurred the queued output bytes are stale and about to
// be superseded by a fresh snapshot reseed, so replaying them would corrupt the
// screen (stale deltas layered on top of the current capture). Control events
// (attention/presence/layout/pane lifecycle) are NOT reseed-recoverable — a
// snapshot only carries pane bytes — so they are preserved and handed back for
// the consumer to deliver after the reseed.
func (s *Sub) Drain() []Event {
	var kept []Event
	for {
		select {
		case ev := <-s.inner.ch:
			if ev.Kind != "output" {
				kept = append(kept, ev)
			}
		default:
			return kept
		}
	}
}

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
	case "subscription-changed":
		c.handleSubscription(rest)
	case "exit":
		c.emit(Notice{Kind: "exit"})
	}
}

// handleSubscription routes a %subscription-changed line from the format
// subscriptions registered in subscribeTitles. Title and command changes become
// notices for the manager; an alternate-screen change is handled here, because
// its only consumer is the attached lenses.
func (c *Controller) handleSubscription(rest string) {
	name, tmuxPane, value, ok := parseSubscription(rest)
	if !ok {
		return
	}
	c.mu.RLock()
	ref := c.byTmuxPane[tmuxPane]
	c.mu.RUnlock()
	if ref == nil {
		return // pane not (yet) registered; a later change re-fires
	}
	switch name {
	case "ccmux-title":
		c.emit(Notice{Kind: "pane-title", PaneID: ref.id, Value: value})
	case "ccmux-cmd":
		c.emit(Notice{Kind: "pane-command", PaneID: ref.id, Value: value})
	case "ccmux-alt":
		c.alternateScreenChanged(ref, value)
	}
}

// parseSubscription splits a %subscription-changed line. Wire format on tmux
// 3.6b (verified):
//
//	name $session @window window-index %pane : value
//
// The value may itself contain " : ", so only the first separator splits.
func parseSubscription(rest string) (name, tmuxPane, value string, ok bool) {
	parts := strings.SplitN(rest, " : ", 2)
	if len(parts) != 2 {
		return "", "", "", false
	}
	f := strings.Fields(parts[0])
	if len(f) < 5 {
		return "", "", "", false
	}
	return f[0], f[4], parts[1], true
}

// alternateScreenChanged asks every attached lens to repaint a pane that just
// left its alternate screen (value "0": a fullscreen program such as Claude
// Code exited or suspended).
//
// Why a repaint is needed at all: a lens emulator mirrors the screen switch
// only if it saw the "enter" sequence go by. A lens seeded by a snapshot while
// the program was already running (attach, reconnect, resize, lag) holds that
// program's screen in its MAIN buffer, so the program's later "leave" restores
// nothing there. tmux restored its own grid correctly, and a fresh capture of
// it is the one screen every lens agrees on. Entering ("1") needs nothing: the
// program repaints itself on the alternate screen anyway.
//
// Only an on-to-off flip repaints. A bare "0" is not one: tmux pushes a pane's
// first value whenever it first evaluates the format, which on 3.4 is lazily on
// the pane's next activity, and on 3.6b also on subscribe. Acting on the value
// alone repainted every bystander lens on panes that had never left the main
// screen (three resize tests caught it, 2026-09-14).
func (c *Controller) alternateScreenChanged(ref *paneRef, value string) {
	on := value == "1"
	c.mu.Lock()
	was := ref.alt
	ref.alt = on
	c.mu.Unlock()
	if was && !on {
		c.fanout(Event{Kind: "repaint", PaneID: ref.id})
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
