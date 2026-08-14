package api

import (
	"sync"
	"time"

	"ccmux.dev/ccmuxd/internal/session"

	"github.com/gorilla/websocket"
)

// resizeSettle is how long a pane must go without another resize before its
// repaint is captured. A lens drags its window one grid cell at a time (the web
// lens debounces at 80ms, the Mac lens not at all), and capturing on every step
// would run a capture-pane per cell for a screen the user is still resizing.
const resizeSettle = 150 * time.Millisecond

// resnapper coalesces "this pane just reflowed, send its screen again" requests
// for ONE attach connection.
//
// Why it exists: tmux only winches the inner program when the size actually
// changes, and a program that repaints on winch does so into %output, which the
// lens already receives. A program that does NOT repaint (a plain shell) leaves
// the reflowed grid on screen with no delta to describe it, so the lens keeps
// showing text wrapped at the old width until something forces a redraw. A fresh
// capture is that redraw, and it costs nothing when no resize is happening.
//
// Pending is a SET, and requesting is LOSSLESS. One attach socket carries every
// pane of a workspace, and a split shows several at once — each with its own
// controller re-asserting its size, so one window-becomes-key or one reconnect
// sends N resizes back to back. Dropping any one of them repaints that pane never
// and leaves it drawn at the old width, which is the bug this exists to fix. The
// set absorbs repeats for free, so the channel carries nothing but a one-slot
// wake-up and no request can be crowded out by another pane's.
//
// Threading: the read goroutine calls `request`; the write goroutine, which is
// the connection's only writer, calls everything else and solely owns the timer.
// `pending` is shared, so it is behind `mu`.
type resnapper struct {
	wake  chan struct{}
	timer *time.Timer

	mu      sync.Mutex
	pending map[string]struct{}
}

func newResnapper() *resnapper {
	// Parked far out rather than left unset: a nil channel in the writer's select
	// would be fine, but a real timer keeps `due()` a plain field read. It cannot
	// have fired yet, so a plain Stop is enough here.
	t := time.NewTimer(time.Hour)
	t.Stop()
	return &resnapper{wake: make(chan struct{}, 1), timer: t, pending: map[string]struct{}{}}
}

// request asks for a repaint of pane. Never blocks and never loses a pane: the id
// goes into the set, and the channel only nudges the writer awake. A nudge that
// finds the slot already full is redundant by definition — the writer has not
// looked at the set yet, and when it does it will see this pane too.
func (r *resnapper) request(pane string) {
	r.mu.Lock()
	r.pending[pane] = struct{}{}
	r.mu.Unlock()
	select {
	case r.wake <- struct{}{}:
	default:
	}
}

// wakeups fires when at least one pane has been requested since the writer last
// looked.
func (r *resnapper) wakeups() <-chan struct{} { return r.wake }

// due fires once the requested panes have gone quiet for resizeSettle.
func (r *resnapper) due() <-chan time.Time { return r.timer.C }

// arm restarts the settle window. The window is shared across panes: a drag that
// moves three of them settles once, then repaints all three.
func (r *resnapper) arm() {
	drainStop(r.timer)
	r.timer.Reset(resizeSettle)
}

// flush repaints every pane whose shared settle window just closed. Runs on the
// write goroutine, which is the only writer this connection has. Returns the
// first write error so the caller can end the loop; a failed capture is per-pane
// and does not stop the others.
//
// Known and accepted: the capture is a control-mode round-trip, and pane output
// produced before it lands keeps queueing on the subscriber. Those bytes are in
// the captured grid AND still in the queue, so they get written again after the
// snapshot — a program streaming output can show a few duplicated lines right
// after a resize. Draining the queue to avoid that would discard output produced
// after the capture, which is not in the snapshot and would be lost for good.
// Duplicated lines are cosmetic and the next redraw clears them; lost output is
// not recoverable, so the duplication is the deliberate side to err on.
func (r *resnapper) flush(conn *websocket.Conn, ctrl *session.Controller) error {
	r.mu.Lock()
	panes := make([]string, 0, len(r.pending))
	for pane := range r.pending {
		panes = append(panes, pane)
	}
	r.pending = map[string]struct{}{}
	r.mu.Unlock()

	for _, pane := range panes {
		if err := sendSnapshot(conn, ctrl, pane); err != nil {
			return err
		}
	}
	return nil
}

// stop is the connection's teardown. Nothing rearms after it — it runs by defer
// once paneWriter.run has returned — so the tick does not need draining.
func (r *resnapper) stop() { r.timer.Stop() }

// drainStop stops a timer and clears any value it already delivered, so a later
// Reset cannot fire immediately on a stale tick. Only `arm` needs it, being the
// only caller that follows with a Reset.
func drainStop(t *time.Timer) {
	if !t.Stop() {
		select {
		case <-t.C:
		default:
		}
	}
}
