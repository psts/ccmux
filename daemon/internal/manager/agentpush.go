package manager

import (
	"context"
	"fmt"
	"time"

	"ccmux.dev/ccmuxd/internal/agent"
)

// PushToAgentPane types a bus message into an opencode agent instance through
// its TUI server; the peers bus calls it for every delivered message and it
// declines panes that are not opencode instances (those get channel pushes
// or poll). Synchronous with a bound: the caller runs it off the bus lock.
func (m *Manager) PushToAgentPane(paneID, text string) error {
	m.mu.RLock()
	_, p := m.findPaneLocked(paneID)
	var startup, name string
	if p != nil {
		startup, name = p.StartupCommand, p.Agent
	}
	m.mu.RUnlock()
	if p == nil || name == "" {
		return fmt.Errorf("not an agent pane")
	}
	port := agent.OpencodePort(startup)
	if port == 0 {
		return fmt.Errorf("agent %s does not take TUI pushes", name)
	}
	if atBareShell(p) {
		return fmt.Errorf("agent %s is asleep", name) // a wake, not a push, is the answer
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	return agent.PushPrompt(ctx, port, text)
}
