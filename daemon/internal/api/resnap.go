package api

import "time"

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
// Threading: `request` is called from the connection's read goroutine; every
// other method belongs to the write goroutine, which owns the timer and is the
// connection's only writer. They meet only at the channel.
type resnapper struct {
	req     chan string
	timer   *time.Timer
	pending string
}

func newResnapper() *resnapper {
	t := time.NewTimer(time.Hour)
	drainStop(t)
	return &resnapper{req: make(chan string, 1), timer: t}
}

// request asks for a repaint of pane. Non-blocking: a full channel already holds
// an unserved request, and the writer re-arms on whichever arrives, so dropping
// one during a drag loses nothing.
func (r *resnapper) request(pane string) {
	select {
	case r.req <- pane:
	default:
	}
}

// requests is the channel the write loop selects on for new requests.
func (r *resnapper) requests() <-chan string { return r.req }

// due fires once a requested pane has gone quiet for resizeSettle.
func (r *resnapper) due() <-chan time.Time { return r.timer.C }

// arm records the pane to repaint and restarts the settle window.
func (r *resnapper) arm(pane string) {
	r.pending = pane
	drainStop(r.timer)
	r.timer.Reset(resizeSettle)
}

// take returns the pane whose settle window just closed, and clears it. Empty
// means the timer fired with nothing pending, which the caller should ignore.
func (r *resnapper) take() string {
	pane := r.pending
	r.pending = ""
	return pane
}

func (r *resnapper) stop() { drainStop(r.timer) }

// drainStop stops a timer and clears any value it already delivered, so a later
// Reset cannot fire immediately on a stale tick.
func drainStop(t *time.Timer) {
	if !t.Stop() {
		select {
		case <-t.C:
		default:
		}
	}
}
