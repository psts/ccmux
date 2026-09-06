package manager

import (
	"context"
	"errors"
	"sync"
	"time"

	"ccmux.dev/ccmuxd/internal/agent"
)

// ErrNotAgentPane says the pane takes no TUI push: not an agent, or a Claude
// instance whose delivery is the channel. Callers stay quiet about it.
var ErrNotAgentPane = errors.New("not a pane that takes TUI pushes")

// ErrAgentAsleep says the instance's pane is at its shell: the message must
// wake it (the bus does, with the text as first prompt) rather than be typed
// into a shell.
var ErrAgentAsleep = errors.New("agent is asleep")

// pushLocks serializes TUI pushes per pane: append-prompt and submit-prompt
// are two calls against one input buffer, and two messages arriving
// together would otherwise interleave into one merged turn.
var pushLocks sync.Map // paneID → *sync.Mutex

func pushLock(paneID string) *sync.Mutex {
	mu, _ := pushLocks.LoadOrStore(paneID, &sync.Mutex{})
	return mu.(*sync.Mutex)
}

// PushToAgentPane types a bus message into an opencode agent instance through
// its TUI server; the peers bus calls it for every delivered message and it
// declines panes that are not opencode instances (those get channel pushes
// or poll). Synchronous with a bound: the caller runs it off the bus lock.
func (m *Manager) PushToAgentPane(paneID, text string) error {
	m.mu.RLock()
	_, p := m.findPaneLocked(paneID)
	var startup, name string
	var asleep bool
	if p != nil {
		startup, name, asleep = p.StartupCommand, p.Agent, atBareShell(p)
	}
	m.mu.RUnlock()
	if p == nil || name == "" {
		return ErrNotAgentPane
	}
	port := agent.OpencodePort(startup)
	if port == 0 {
		return ErrNotAgentPane // a Claude instance: channel push, not TUI
	}
	_ = name
	if asleep {
		return ErrAgentAsleep // a wake, not a push, is the answer
	}
	mu := pushLock(paneID)
	mu.Lock()
	defer mu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	return agent.PushPrompt(ctx, port, text)
}
