package api

import (
	"sync"

	"github.com/gorilla/websocket"
)

// noticeQueue holds short, user-facing messages for ONE attach connection.
//
// It exists because the thing worth telling the user about — a paste that was
// cut short — is discovered on the pane's SENDER goroutine, and that goroutine
// must never touch the websocket: paneWriter is the connection's only writer,
// and a second one would interleave frames mid-JSON. Same split as resnapper:
// a background goroutine posts, the writer drains.
//
// Pending is a MAP keyed by pane, so a pane's newest message replaces its older
// one, and no pane can crowd out another's. A dying control connection fails
// every queued job on a pane, and each one now produces a notice — total
// failures stopped being silent once it turned out they are invisible at a
// password prompt — so without the map a single death would post one line per
// queued job. Fifty copies of the same sentence is not fifty times the
// information.
//
// Threading: post is called from sender goroutines; wakeups/drain/flush belong
// to the write goroutine. pending is shared, so it is behind mu.
type noticeQueue struct {
	wake chan struct{}

	mu      sync.Mutex
	pending map[string]string // pane id -> newest message
}

func newNoticeQueue() *noticeQueue {
	return &noticeQueue{wake: make(chan struct{}, 1), pending: map[string]string{}}
}

// post queues a message for pane. Never blocks: it runs on a pane's sender
// goroutine, which is shared by every lens attached to that pane, so waiting
// on one slow socket here would stall input for all of them.
func (q *noticeQueue) post(pane, msg string) {
	q.mu.Lock()
	q.pending[pane] = msg
	q.mu.Unlock()
	select {
	case q.wake <- struct{}{}:
	default: // already nudged; the writer has not read the map yet and will see this
	}
}

func (q *noticeQueue) wakeups() <-chan struct{} { return q.wake }

// flush writes and clears everything queued. Write-goroutine only.
func (q *noticeQueue) flush(conn *websocket.Conn) error {
	q.mu.Lock()
	pending := q.pending
	q.pending = map[string]string{}
	q.mu.Unlock()

	for pane, msg := range pending {
		if err := conn.WriteJSON(wsMsg{T: "notice", Pane: pane, Notice: msg}); err != nil {
			return err
		}
	}
	return nil
}
