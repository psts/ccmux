package tmux

import "sync"

// maxQueuedBytesPerPane bounds how much unsent input one pane may hold. Past
// it, submitting blocks until the worker drains enough — real backpressure
// rather than unbounded daemon memory. Input is never dropped: a keystroke the
// user typed and we discarded is worse than a slow one.
//
// 8 MB is far above any real paste (the case this exists for was 19 kB) and
// still bounded per pane, so a runaway lens costs memory it cannot exceed.
const maxQueuedBytesPerPane = 8 << 20

// sendJob is one submitted send. done, when set, is called with the delivery
// result once the bytes are actually in tmux — off the submitter's goroutine.
type sendJob struct {
	data []byte
	done func(error)
}

// paneSender is the FIFO of pending sends for ONE pane, and the reason input
// ordering survives being asynchronous.
//
// It replaced a per-pane mutex, which looked equivalent and was not:
// sync.Mutex is not FIFO, so two senders racing on a pane got an arbitrary
// order. Per-lens keystroke order used to come from api.readLoop calling
// SendInput sequentially AND BLOCKING, so frame N was in tmux before frame N+1
// was read off the socket. The moment that call stopped blocking, that was the
// only thing supplying the order — hence a real queue, and hence both the
// synchronous and asynchronous entry points going through this one.
//
// What it guarantees is ENQUEUE order, which equals submission order for any
// ONE submitting goroutine — the case that matters, since a lens has exactly
// one read goroutine. It is not a total order across submitters: two callers
// parked on `room` are woken by one Broadcast and then race for the mutex, and
// neither Cond wakeup nor Mutex acquisition is FIFO. Order between two
// different sources to one pane was never defined and still is not.
//
// The worker exits when the queue drains and is restarted on the next submit,
// so an idle pane costs no goroutine. Client has no pane-close signal, so a
// worker that lived until the connection died would be a goroutine per pane
// the session ever touched.
// It takes a send func rather than the Client and pane it belongs to: it needs
// nothing else from either, and the narrower seam is what lets the byte bound
// and the ordering guarantee be tested without a tmux server.
type paneSender struct {
	send  func([]byte) error // one send, already bound to its pane
	limit int                // queued-byte bound; a field so tests can shrink it

	mu      sync.Mutex
	room    *sync.Cond // signalled as queued bytes drop
	queue   []sendJob
	bytes   int
	running bool
}

func newPaneSender(limit int, send func([]byte) error) *paneSender {
	s := &paneSender{send: send, limit: limit}
	s.room = sync.NewCond(&s.mu)
	return s
}

// submit appends a job, blocking only while the pane is over its byte bound,
// and starts the worker if it is not already running.
func (s *paneSender) submit(j sendJob) {
	s.mu.Lock()
	// The `s.bytes > 0` term matters: a single job larger than the whole bound
	// must still go through, or it would wait for room that can never appear.
	for s.bytes > 0 && s.bytes+len(j.data) > s.limit {
		s.room.Wait()
	}
	s.queue = append(s.queue, j)
	s.bytes += len(j.data)
	if !s.running {
		s.running = true
		go s.drain()
	}
	s.mu.Unlock()
}

// drain sends queued jobs one whole job at a time, in order, until the queue is
// empty. Taking a job whole is what keeps a paste contiguous: another lens's
// keystroke can land before or after it, never inside it.
//
// A closed connection needs no special case — Command returns ErrClosed at
// once, so the queue empties fast with every waiter told why.
func (s *paneSender) drain() {
	for {
		s.mu.Lock()
		if len(s.queue) == 0 {
			s.running = false // next submit starts a fresh worker
			s.mu.Unlock()
			return
		}
		j := s.queue[0]
		s.queue = s.queue[1:]
		s.bytes -= len(j.data)
		s.room.Broadcast()
		s.mu.Unlock()

		err := s.send(j.data)
		if j.done != nil {
			j.done(err)
		}
	}
}
