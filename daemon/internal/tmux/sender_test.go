package tmux

import (
	"errors"
	"sync"
	"testing"
	"time"
)

// recorder is a fake send that records what it was handed, in order.
type recorder struct {
	mu    sync.Mutex
	got   [][]byte
	delay time.Duration
	err   error
}

func (r *recorder) send(data []byte) error {
	if r.delay > 0 {
		time.Sleep(r.delay)
	}
	r.mu.Lock()
	r.got = append(r.got, append([]byte(nil), data...))
	r.mu.Unlock()
	return r.err
}

func (r *recorder) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.got)
}

// TestPaneSender_PreservesSubmissionOrder pins the guarantee that replaced the
// blocking call. Per-lens keystroke order used to come from api.readLoop
// calling SendInput sequentially and waiting; once it stopped waiting, this
// queue became the only thing supplying it. A mutex would NOT do — sync.Mutex
// is not FIFO — which is why this is a queue and why this test exists.
func TestPaneSender_PreservesSubmissionOrder(t *testing.T) {
	r := &recorder{}
	s := newPaneSender(1<<20, r.send)

	const n = 200
	var wg sync.WaitGroup
	wg.Add(n)
	for i := 0; i < n; i++ {
		s.submit(sendJob{data: []byte{byte(i)}, done: func(error) { wg.Done() }})
	}
	wg.Wait()

	if r.count() != n {
		t.Fatalf("delivered %d jobs, want %d", r.count(), n)
	}
	for i, got := range r.got {
		if got[0] != byte(i) {
			t.Fatalf("job %d delivered out of order: got %d", i, got[0])
		}
	}
}

// TestPaneSender_BoundBlocksInsteadOfGrowing pins backpressure. Without the
// bound a lens that submits faster than tmux drains grows daemon memory with
// nothing to stop it or surface it; the chosen trade is that the submitter
// waits rather than that input is dropped.
//
// It asserts on ELAPSED TIME, not on a sampled queue depth. An earlier version
// checked s.bytes after every submit returned, by which point the queue has
// drained — and submit's own wait loop makes bytes > limit unreachable anyway,
// so that assertion could not fail. Time is what the bound actually costs, so
// time is what to measure: six jobs that cannot be queued at once must wait
// for sends to finish, and without the wait loop the loop returns immediately.
func TestPaneSender_BoundBlocksInsteadOfGrowing(t *testing.T) {
	const sendDelay = 30 * time.Millisecond
	r := &recorder{delay: sendDelay}
	s := newPaneSender(10, r.send) // tiny bound so the test does not need 8 MB

	done := make(chan time.Duration, 1)
	go func() {
		start := time.Now()
		for i := 0; i < 6; i++ {
			s.submit(sendJob{data: make([]byte, 4)}) // 24 bytes total, bound 10
		}
		done <- time.Since(start)
	}()

	var elapsed time.Duration
	select {
	case elapsed = <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("submitter never made progress — the bound deadlocked")
	}

	// Only two 4-byte jobs fit under a 10-byte bound, so at least three of the
	// six submits must wait for a send to complete. Three delays is the floor;
	// asserting two keeps margin on a loaded host while still being far above
	// the ~0 an unbounded queue would take.
	if min := 2 * sendDelay; elapsed < min {
		t.Errorf("six jobs past a %d-byte bound took %v, want at least %v — "+
			"returning that fast means the bound is not enforced", s.limit, elapsed, min)
	}

	deadline := time.Now().Add(5 * time.Second)
	for r.count() < 6 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if r.count() != 6 {
		t.Fatalf("delivered %d of 6 — blocked submitters must still drain", r.count())
	}
}

// TestPaneSender_OversizedJobStillGoesThrough guards the `s.bytes > 0` term in
// submit's wait condition. Without it a single job bigger than the whole bound
// waits for room that can never appear, and one large paste hangs the pane for
// good.
func TestPaneSender_OversizedJobStillGoesThrough(t *testing.T) {
	r := &recorder{}
	s := newPaneSender(8, r.send)

	delivered := make(chan error, 1)
	s.submit(sendJob{data: make([]byte, 4096), done: func(e error) { delivered <- e }})
	select {
	case <-delivered:
	case <-time.After(5 * time.Second):
		t.Fatal("a job larger than the bound never ran — submit waits for room " +
			"that cannot appear")
	}
}

// TestPaneSender_WorkerExitsWhenDrained pins the lifecycle. Client has no
// pane-close signal, so a worker that ran until the connection died would be a
// permanent goroutine per pane the session ever touched.
func TestPaneSender_WorkerExitsWhenDrained(t *testing.T) {
	r := &recorder{}
	s := newPaneSender(1<<20, r.send)

	first := make(chan error, 1)
	s.submit(sendJob{data: []byte("a"), done: func(e error) { first <- e }})
	<-first

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		s.mu.Lock()
		running := s.running
		s.mu.Unlock()
		if !running {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	s.mu.Lock()
	running := s.running
	s.mu.Unlock()
	if running {
		t.Fatal("worker did not exit after draining — one goroutine leaks per pane")
	}

	// And a later submit must restart it rather than queue into nothing.
	second := make(chan error, 1)
	s.submit(sendJob{data: []byte("b"), done: func(e error) { second <- e }})
	select {
	case <-second:
	case <-time.After(5 * time.Second):
		t.Fatal("submit after the worker exited never ran — the queue is dead")
	}
}

// TestPaneSender_ReportsSendErrorToItsOwnJob keeps the error attached to the
// submission it belongs to, which is what lets applyInput log the right pane
// and withhold presence.
func TestPaneSender_ReportsSendErrorToItsOwnJob(t *testing.T) {
	want := errors.New("boom")
	r := &recorder{err: want}
	s := newPaneSender(1<<20, r.send)

	got := make(chan error, 1)
	s.submit(sendJob{data: []byte("x"), done: func(e error) { got <- e }})
	if err := <-got; !errors.Is(err, want) {
		t.Fatalf("done got %v, want %v", err, want)
	}
}
