package manager

import (
	"context"
	"errors"
	"fmt"
	"time"

	"ccmux.dev/ccmuxd/internal/agent"
)

// ErrNotAgentPane says the pane takes no TUI push: not an agent, or a Claude
// instance whose delivery is the channel. Callers stay quiet about it.
var ErrNotAgentPane = errors.New("not a pane that takes TUI pushes")

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
	if asleep {
		return fmt.Errorf("agent %s is asleep", name) // a wake, not a push, is the answer
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	return agent.PushPrompt(ctx, port, text)
}
