// Package session owns the live control-mode connection for one tmux session
// (= one ccmux workspace) and maps ccmux panes to tmux windows. It is the only
// tmux client for its session; lenses subscribe here for pane output and issue
// input/resize through it.
package session

import (
	"context"
	"fmt"
	"strings"
	"sync"

	"ccmux.dev/ccmuxd/internal/model"
	"ccmux.dev/ccmuxd/internal/tmux"
)

// Event is a message fanned out to attached lenses. Kind "output" carries pane
// bytes in Data; "attention" carries a pane's new Attention; lifecycle kinds
// ("pane-added"/"pane-closed") carry just the PaneID. All lens-bound traffic
// flows through this one channel so ordering per subscriber is preserved.
type Event struct {
	Kind      string
	PaneID    string
	Data      []byte
	Attention model.Attention
}

// paneRef binds a stable ccmux pane id to its runtime tmux window/pane ids.
type paneRef struct {
	id     string // ccmux uuid
	window string // @N
	pane   string // %N
}

// Notice is a non-output control-mode event surfaced to the manager (window
// lifecycle, session exit) so it can update the registry and notify lenses.
type Notice struct {
	Kind   string // "window-close" | "exit"
	Window string // @N when applicable
	PaneID string // ccmux pane id when resolvable
}

// Controller manages a single session's control connection.
type Controller struct {
	server  *tmux.Server
	session string
	wsID    string
	client  *tmux.Client

	mu          sync.RWMutex
	byTmuxPane  map[string]*paneRef // "%0" -> ref
	byID        map[string]*paneRef // uuid -> ref
	subs        map[int]*subscriber
	nextSub     int
	notices     chan Notice
	closed      bool
}

// Open dials control mode for an existing session, stamps the session's managed
// metadata, and discovers any windows already carrying an @ccmux_pane_id (used
// both for fresh sessions and for adopting sessions after a daemon restart).
func Open(ctx context.Context, server *tmux.Server, session, wsID string) (*Controller, error) {
	c := &Controller{
		server:     server,
		session:    session,
		wsID:       wsID,
		byTmuxPane: map[string]*paneRef{},
		byID:       map[string]*paneRef{},
		subs:       map[int]*subscriber{},
		notices:    make(chan Notice, 64),
	}
	client, err := tmux.Dial(ctx, server.Socket, session, c)
	if err != nil {
		return nil, err
	}
	c.client = client
	if err := c.stampSession(); err != nil {
		client.Close()
		return nil, err
	}
	if err := c.discover(); err != nil {
		client.Close()
		return nil, err
	}
	return c, nil
}

func (c *Controller) stampSession() error {
	if _, err := c.client.Command("set-option", "-t", c.session, "@ccmux_managed", "1"); err != nil {
		return err
	}
	if _, err := c.client.Command("set-option", "-t", c.session, "@ccmux_workspace_id", c.wsID); err != nil {
		return err
	}
	// Per-session (never global): the daemon drives sizing via resize-window.
	// Setting this globally crashes session creation on tmux 3.6b; scoping it to
	// an already-existing session is safe.
	_, err := c.client.Command("set-option", "-t", c.session, "window-size", "manual")
	return err
}

// discover reads existing windows and registers those already tagged with a
// pane id.
func (c *Controller) discover() error {
	lines, err := c.client.Command("list-windows", "-t", c.session, "-F",
		"#{@ccmux_pane_id}|#{window_id}|#{pane_id}")
	if err != nil {
		return err
	}
	for _, ln := range lines {
		f := strings.SplitN(ln, "|", 3)
		if len(f) != 3 || f[0] == "" {
			continue
		}
		c.register(&paneRef{id: f[0], window: f[1], pane: f[2]})
	}
	return nil
}

func (c *Controller) register(ref *paneRef) {
	c.mu.Lock()
	c.byTmuxPane[ref.pane] = ref
	c.byID[ref.id] = ref
	c.mu.Unlock()
}

// FirstWindow returns the sole window's ids for a freshly created session, so
// the manager can adopt it as pane 0.
func (c *Controller) FirstWindow() (window, pane string, err error) {
	lines, err := c.client.Command("list-windows", "-t", c.session, "-F", "#{window_id}|#{pane_id}")
	if err != nil {
		return "", "", err
	}
	if len(lines) == 0 {
		return "", "", fmt.Errorf("session %s has no windows", c.session)
	}
	f := strings.SplitN(lines[0], "|", 2)
	return f[0], f[1], nil
}

// AdoptWindow stamps an existing window with a ccmux pane id and registers it.
func (c *Controller) AdoptWindow(paneID, window, pane string) error {
	if _, err := c.client.Command("set-option", "-w", "-t", window, "@ccmux_pane_id", paneID); err != nil {
		return err
	}
	c.register(&paneRef{id: paneID, window: window, pane: pane})
	return nil
}

// SpawnWindow creates a new window (= new ccmux pane) at cwd with env, stamps it
// with paneID, and registers it.
func (c *Controller) SpawnWindow(paneID, cwd string, env map[string]string) error {
	args := []string{"new-window", "-t", c.session, "-P", "-F", "#{window_id}|#{pane_id}"}
	if cwd != "" {
		args = append(args, "-c", cwd)
	}
	for k, v := range env {
		args = append(args, "-e", k+"="+v)
	}
	lines, err := c.client.Command(args...)
	if err != nil || len(lines) == 0 {
		return fmt.Errorf("new-window: %v", err)
	}
	f := strings.SplitN(strings.TrimSpace(lines[0]), "|", 2)
	if len(f) != 2 {
		return fmt.Errorf("new-window returned %q", lines[0])
	}
	if _, err := c.client.Command("set-option", "-w", "-t", f[0], "@ccmux_pane_id", paneID); err != nil {
		return err
	}
	c.register(&paneRef{id: paneID, window: f[0], pane: f[1]})
	return nil
}

// SendInput injects raw bytes into a pane (used for keystrokes and delivering a
// startup command).
func (c *Controller) SendInput(paneID string, data []byte) error {
	ref := c.ref(paneID)
	if ref == nil {
		return fmt.Errorf("unknown pane %s", paneID)
	}
	return c.client.SendKeys(ref.pane, data)
}

// Resize sets a pane's window size.
func (c *Controller) Resize(paneID string, cols, rows int) error {
	ref := c.ref(paneID)
	if ref == nil {
		return fmt.Errorf("unknown pane %s", paneID)
	}
	return c.client.ResizeWindow(ref.window, cols, rows)
}

// Capture returns a pane's current contents (escape-preserving) for seeding a
// fresh attach.
func (c *Controller) Capture(paneID string, historyLines int) ([]byte, error) {
	ref := c.ref(paneID)
	if ref == nil {
		return nil, fmt.Errorf("unknown pane %s", paneID)
	}
	return c.client.CapturePane(ref.pane, historyLines)
}

// KillPane removes a pane's window.
func (c *Controller) KillPane(paneID string) error {
	ref := c.ref(paneID)
	if ref == nil {
		return fmt.Errorf("unknown pane %s", paneID)
	}
	_, err := c.client.Command("kill-window", "-t", ref.window)
	return err
}

func (c *Controller) ref(paneID string) *paneRef {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.byID[paneID]
}

// Notices returns the channel of window-lifecycle / exit events.
func (c *Controller) Notices() <-chan Notice { return c.notices }

// Close ends the control connection (the tmux session survives).
func (c *Controller) Close() error {
	c.mu.Lock()
	c.closed = true
	c.mu.Unlock()
	return c.client.Close()
}
