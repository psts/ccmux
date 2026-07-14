package manager

import (
	"sync"

	"ccmux.dev/ccmuxd/internal/model"
)

// firehoseBuffer bounds a firehose subscriber's pending-event queue. The firehose
// is advisory — it flashes sidebar attention for lenses that are NOT holding a
// full attach WebSocket on a workspace — so an overflowing (slow) subscriber may
// safely drop events; a lens re-syncs its true state via GET /v1/workspaces.
const firehoseBuffer = 256

// Event is a global, workspace-scoped notification delivered to /v1/events
// firehose subscribers. Unlike session.Event (fanned out only to lenses attached
// to one workspace), a firehose Event names its WorkspaceID so a sidebar lens can
// flash the right row without being attached to it.
type Event struct {
	// Kind is the discriminator: "attention" today; workspace-lifecycle kinds
	// (added/removed/status) slot in here without touching the transport.
	Kind        string
	WorkspaceID string
	PaneID      string          // set for "attention"
	Attention   model.Attention // set for "attention"
}

// firehose is a non-blocking pub/sub hub for global Events. It mirrors the
// controller fan-out's drop-on-overflow discipline: publishers (the manager,
// running on hook/control goroutines) must never block on a slow lens.
type firehose struct {
	mu   sync.Mutex
	subs map[int]chan Event
	next int
}

func newFirehose() *firehose { return &firehose{subs: map[int]chan Event{}} }

// subscribe registers a consumer and returns its id and receive channel. The
// channel is closed by unsubscribe.
func (f *firehose) subscribe() (int, <-chan Event) {
	f.mu.Lock()
	defer f.mu.Unlock()
	id := f.next
	f.next++
	ch := make(chan Event, firehoseBuffer)
	f.subs[id] = ch
	return id, ch
}

// unsubscribe removes a consumer and closes its channel. Idempotent.
func (f *firehose) unsubscribe(id int) {
	f.mu.Lock()
	if ch, ok := f.subs[id]; ok {
		delete(f.subs, id)
		close(ch)
	}
	f.mu.Unlock()
}

// publish fans an event out to every subscriber without blocking; a subscriber
// whose buffer is full drops the event (see firehoseBuffer). Holding the lock for
// the whole loop is what makes closing a channel in unsubscribe safe: a delivery
// and a close can never interleave on the same channel.
func (f *firehose) publish(ev Event) {
	f.mu.Lock()
	for _, ch := range f.subs {
		select {
		case ch <- ev:
		default:
		}
	}
	f.mu.Unlock()
}
