package meridian

import (
	"context"
	"log"
	"os/exec"
	"sync"
	"time"
)

// Backoff bounds for restarting a sidecar that exits on its own.
const (
	restartMin = time.Second
	restartMax = 30 * time.Second
	// healthyRun is how long a child must have lived for its next crash to
	// restart at restartMin again instead of the ceiling it climbed to.
	healthyRun = time.Minute
)

// proc is one supervised child and its restart loop.
type proc struct {
	spec   Spec
	bin    func() (string, error)
	ctx    context.Context
	cancel context.CancelFunc
	done   chan struct{} // closed when run() returns, i.e. the child is gone

	mu       sync.Mutex
	cmd      *exec.Cmd
	since    time.Time
	restarts int
	lastErr  string
}

func newProc(parent context.Context, bin func() (string, error), s Spec) *proc {
	ctx, cancel := context.WithCancel(parent)
	return &proc{spec: s, bin: bin, ctx: ctx, cancel: cancel, done: make(chan struct{})}
}

// run starts the child and restarts it with capped backoff until stop().
func (p *proc) run() {
	defer close(p.done)
	delay := restartMin
	for {
		started := time.Now()
		err := p.once()
		if p.ctx.Err() != nil {
			return
		}
		if time.Since(started) > healthyRun {
			delay = restartMin // a long healthy run earns a fresh backoff
		}
		p.mu.Lock()
		p.restarts++
		p.lastErr = ""
		if err != nil {
			p.lastErr = err.Error()
		}
		p.mu.Unlock()
		log.Printf("meridian[%s]: exited (%v); restarting in %s", p.spec.Name, err, delay)
		select {
		case <-time.After(delay):
		case <-p.ctx.Done():
			return
		}
		if delay *= 2; delay > restartMax {
			delay = restartMax
		}
	}
}

// once runs the child to completion. A start failure (binary missing) is
// returned like an exit so the loop keeps trying: installing Meridian while
// the daemon runs is the expected order for a fresh host.
func (p *proc) once() error {
	bin, err := p.bin()
	if err != nil {
		return err
	}
	cmd, err := Command(p.ctx, bin, p.spec)
	if err != nil {
		return err
	}
	if err := cmd.Start(); err != nil {
		return err
	}
	p.mu.Lock()
	p.cmd, p.since, p.lastErr = cmd, time.Now(), ""
	p.mu.Unlock()
	log.Printf("meridian[%s]: started pid %d at %s", p.spec.Name, cmd.Process.Pid, p.spec.BaseURL)
	err = cmd.Wait()
	p.mu.Lock()
	p.cmd = nil
	p.mu.Unlock()
	return err
}

// stop ends the child and the restart loop, and returns only once the child
// has exited: a replacement started before then would race it for the port
// and lose (EADDRINUSE), showing up as a restart and a stale lastError.
func (p *proc) stop() {
	p.cancel()
	<-p.done
}

func (p *proc) status() Status {
	p.mu.Lock()
	defer p.mu.Unlock()
	st := Status{Restarts: p.restarts, LastError: p.lastErr}
	if p.cmd != nil && p.cmd.Process != nil {
		st.Running, st.PID, st.Since = true, p.cmd.Process.Pid, p.since
	}
	return st
}
