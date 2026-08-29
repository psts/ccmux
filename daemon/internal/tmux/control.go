package tmux

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"strings"
	"sync"
)

// ErrClosed is returned by Command once the control connection has ended.
var ErrClosed = errors.New("tmux: control connection closed")

// Handler receives asynchronous control-mode notifications. Its methods are
// invoked serially from the client's single reader goroutine and must not block
// (hand work to a channel/goroutine if needed).
type Handler interface {
	// OnOutput delivers already-unescaped pane bytes from a %output line.
	OnOutput(pane string, data []byte)
	// OnNotification delivers any other %-notification: kind is the token after
	// the leading '%' (e.g. "window-add", "window-close", "layout-change",
	// "exit", "session-changed"); rest is the remainder of the line.
	OnNotification(kind, rest string)
}

// Client is a single tmux control-mode connection (`tmux -L <socket> -C attach`).
// It is the only tmux client for its server; all pane I/O and commands flow
// through it in-band (no per-command fork/exec), honoring the project's
// no-process-spawning-in-hot-paths rule.
type Client struct {
	socket  string
	cmd     *exec.Cmd
	stdin   io.WriteCloser
	handler Handler

	mu      sync.Mutex
	pending []chan reply // FIFO: control mode answers commands in send order
	closed  bool
	done    chan struct{}

	// wmu makes "queue the reply channel" and "write the command" one step.
	// pending is a FIFO matched to send order, so two callers that append in
	// one order and reach the write in the other hand each other's reply back
	// — a resize answering a capture-pane. Chunked SendKeys made that window
	// wider, but the race predates it.
	//
	// Deliberately NOT mu: holding mu across the write would block the reader
	// goroutine in deliver, tmux's stdout pipe would fill, its single event
	// loop would stop reading our stdin, and the write would never finish.
	wmu sync.Mutex

	// sendLocks keeps one SendKeys' chunks contiguous, PER PANE (see
	// commands.go). Per pane and not one global lock because a chunked send is
	// not quick: measured against tmux 3.4, a 1 MB paste is ~3s at the current
	// chunk size. Not a constant — the cost is quadratic in chunk size, from
	// 2.2s to 26.9s across the range; commands.go has the table and is the
	// authority. A global lock would freeze typing in every other pane for
	// that whole time, and panes here are shared between lenses.
	//
	// This isolates panes ACROSS connections. It does not make a big send
	// cheap for the connection that issued it: api.applyInput calls SendKeys
	// inline on the attach read goroutine, so that lens still blocks on its
	// own paste. Fixing that needs a sender queue, not a lock.
	//
	// Entries are never removed. One pointer per pane id the session has ever
	// used is a few hundred bytes over a daemon's life, and reference counting
	// them would need a lock held across the send anyway.
	sendmu    sync.Mutex // guards sendLocks only, never held across a send
	sendLocks map[string]*sync.Mutex

	// reapErr is the child's exit status, written by shutdown() and readable
	// only after reaped closes.
	//
	// History, because it explains the shape: Wait() used to live only in
	// Close(), so a client whose tmux died on its own — session killed, last
	// pane exited, server crashed — was never reaped at all. markCold used to
	// drop the controller without closing it, so nothing reached Close, and
	// the dead process stayed a zombie for the daemon's whole life. See the
	// package doc of internal/childproc for the incident.
	//
	// shutdown() is the one funnel EVERY death already flows through, so the
	// reap lives there and there only — a single call site is the only place
	// its precondition ("readLoop has stopped, so no read from the stdout
	// pipe Wait is about to close is still in flight") can be proven.
	// markCold closes too, but that is NOT what fixes the zombie; it releases
	// the controller and ends a client that is still alive on a spurious
	// %exit. Deleting either one does not restore the leak on its own.
	reapErr error
	// reaped closes once Wait has returned. done cannot serve this purpose:
	// it is closed FIRST so that Command callers blocked on a dead connection
	// get ErrClosed immediately, without waiting on a Wait that a process
	// which closed stdout but has not exited could stall forever.
	// So done means "the connection ended", reaped means "the child is gone".
	reaped chan struct{}
}

// paneSend returns the lock serializing sends to one pane, creating it on first
// use.
func (c *Client) paneSend(pane string) *sync.Mutex {
	c.sendmu.Lock()
	defer c.sendmu.Unlock()
	m := c.sendLocks[pane]
	if m == nil {
		m = &sync.Mutex{}
		c.sendLocks[pane] = m
	}
	return m
}

type reply struct {
	lines []string
	err   error
}

// Dial starts a control-mode client attached to session on the given tmux
// socket (-L). The server must already exist. h receives notifications.
func Dial(ctx context.Context, socket, session string, h Handler) (*Client, error) {
	cmd := exec.CommandContext(ctx, "tmux", "-L", socket, "-C", "attach", "-t", session)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	cmd.Stderr = nil
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	c := &Client{socket: socket, cmd: cmd, stdin: stdin, handler: h, done: make(chan struct{}),
		reaped: make(chan struct{}), sendLocks: map[string]*sync.Mutex{}}
	// tmux answers the implicit `attach` command with an initial %begin/%end
	// block that we never issued. Pre-queue a discard slot (before the reader
	// starts) so the FIFO stays aligned with caller-issued commands.
	c.pending = append(c.pending, make(chan reply, 1))
	go c.readLoop(stdout)
	return c, nil
}

// Done is closed when the control connection ends (tmux %exit or stdout EOF).
func (c *Client) Done() <-chan struct{} { return c.done }

func (c *Client) readLoop(stdout io.Reader) {
	br := bufio.NewReaderSize(stdout, 1<<16)
	var (
		inReply bool
		lines   []string
	)
	for {
		raw, err := br.ReadBytes('\n')
		if len(raw) > 0 {
			line := strings.TrimSuffix(string(raw), "\n")
			inReply, lines = c.handleLine(line, inReply, lines)
		}
		if err != nil {
			break
		}
	}
	c.shutdown()
}

// handleLine processes one control-stream line, threading the reply-collection
// state (inReply / accumulated lines) through so the reader loop stays flat.
func (c *Client) handleLine(line string, inReply bool, lines []string) (bool, []string) {
	if inReply {
		switch {
		case strings.HasPrefix(line, "%end "):
			c.deliver(reply{lines: lines})
			return false, nil
		case strings.HasPrefix(line, "%error "):
			c.deliver(reply{lines: lines, err: fmt.Errorf("tmux: %s", strings.Join(lines, "; "))})
			return false, nil
		default:
			return true, append(lines, line)
		}
	}
	switch {
	case strings.HasPrefix(line, "%begin "):
		return true, nil
	case strings.HasPrefix(line, "%output "):
		pane, data := splitPaneData(line[len("%output "):])
		c.handler.OnOutput(pane, UnescapeOutput([]byte(data)))
	case strings.HasPrefix(line, "%"):
		kind, rest, _ := strings.Cut(line[1:], " ")
		c.handler.OnNotification(kind, rest)
	}
	return false, lines
}

// splitPaneData splits `%N <data>` into the pane id and its (still-escaped) data.
func splitPaneData(s string) (pane, data string) {
	if i := strings.IndexByte(s, ' '); i >= 0 {
		return s[:i], s[i+1:]
	}
	return s, ""
}

func (c *Client) deliver(r reply) {
	c.mu.Lock()
	if len(c.pending) == 0 {
		c.mu.Unlock()
		return // stray reply block; nothing waiting
	}
	ch := c.pending[0]
	c.pending = c.pending[1:]
	c.mu.Unlock()
	ch <- r
}

// Command sends a tmux command and blocks until its reply block completes,
// returning the raw reply lines (verbatim; command replies are not escaped).
func (c *Client) Command(args ...string) ([]string, error) {
	line, err := formatCommand(args)
	if err != nil {
		return nil, err
	}
	ch := make(chan reply, 1)
	c.wmu.Lock()
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		c.wmu.Unlock()
		return nil, ErrClosed
	}
	c.pending = append(c.pending, ch)
	c.mu.Unlock()

	_, werr := io.WriteString(c.stdin, line+"\n")
	c.wmu.Unlock()
	if werr != nil {
		// Take the slot back. pending is a strict FIFO matched to send order,
		// so an orphan left here makes the NEXT command's reply pop into this
		// dead channel and every caller after it one reply behind — the exact
		// desync wmu exists to prevent. In practice a write error means the
		// pipe is gone and shutdown drains pending anyway, but Command has no
		// timeout: if that ever does not hold, every command on this
		// connection blocks with no error and no log.
		// Match on identity, not position — deliver may already have consumed
		// earlier entries.
		c.mu.Lock()
		for i, p := range c.pending {
			if p == ch {
				c.pending = append(c.pending[:i], c.pending[i+1:]...)
				break
			}
		}
		c.mu.Unlock()
		return nil, werr
	}
	select {
	case r := <-ch:
		return r.lines, r.err
	case <-c.done:
		return nil, ErrClosed
	}
}

func (c *Client) shutdown() {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return
	}
	c.closed = true
	pending := c.pending
	c.pending = nil
	c.mu.Unlock()

	close(c.done)
	for _, ch := range pending {
		ch <- reply{err: ErrClosed}
	}
	c.handler.OnNotification("exit", "")

	// Reap LAST. Wait can stall on a process that closed stdout without
	// exiting — that hazard is why done closes first — and nothing above this
	// line may sit behind it. The exit notice is what drives markCold, so
	// reaping ahead of it would leave a workspace reading live in both lenses
	// with a dead connection under it: stale state with nothing saying so,
	// the same invisibility this whole change set out to end.
	//
	// Safe here and only here: readLoop has stopped, so every read from the
	// stdout pipe that Wait is about to close has completed. shutdown's
	// closed-guard means this body runs exactly once, so Wait does too.
	c.reapErr = c.cmd.Wait()
	close(c.reaped)
}

// Close ends the control connection and its tmux client (the session survives
// on the server — this is detach, not kill). Idempotent, and safe to call on a
// client that already died on its own: it returns that same exit status.
func (c *Client) Close() error {
	_ = c.stdin.Close()
	if c.cmd.Process != nil {
		_ = c.cmd.Process.Kill()
	}
	// Wait for shutdown() to finish, reap included. Close does NOT call Wait
	// itself: that would be a second site where the "no reads still in
	// flight" precondition has to hold, and the exported one, where callers
	// cannot know the rule. The Kill above guarantees the EOF that ends
	// readLoop and gets shutdown here.
	<-c.reaped
	return c.reapErr
}
