package manager

import (
	"fmt"

	"ccmux.dev/ccmuxd/internal/model"
)

// The bus's spawn_if_missing starts a repo's Claude in that repo's own
// session (StartClaudeIn): in an open one as a new pane, in a closed one by
// reopening it the way Revive does, with Claude started in the pane where
// it ran before so the session does not fill with dead shells.

// repoNamed reports whether a repo workspace answers to name on the bus:
// its folder's name, which is what its Claude registers as and what the
// window list shows. Not the workspace's own name: a spawn waits for a
// Claude registering under the name it was asked for, and one started by
// the workspace's name would register under the folder's and never arrive.
func repoNamed(ws *model.Workspace, name string) bool {
	return name != "" && baseName(ws.RepoPath) == name
}

// ClosedWorkspaceForRepo finds a closed repo workspace in group that
// answers to name.
func (m *Manager) ClosedWorkspaceForRepo(group, name string) (wsID, repoPath string, ok bool) {
	st := m.windowSnapshot()
	m.mu.RLock()
	defer m.mu.RUnlock()
	for _, e := range m.byID {
		if e.ctrl != nil || e.ws.Agent != "" || st.resolve(e.ws.ID, e.ws.Group) != group {
			continue
		}
		if repoNamed(e.ws, name) {
			return e.ws.ID, e.ws.RepoPath, true
		}
	}
	return "", "", false
}

// StartClaudeIn starts cmd (a teammate's Claude) in a repo's session,
// deciding open or closed at this moment rather than when the bus looked:
// a closed session is reopened like Revive (every pane with its own
// start-up command) with cmd typed into the pane Claude last ran in; an
// open one (including one opened by someone else since the bus looked),
// or a reopened one with no Claude pane to reuse, gets a new pane for cmd.
func (m *Manager) StartClaudeIn(wsID, cmd string) error {
	m.mu.RLock()
	e := m.byID[wsID]
	var claude string
	if e != nil {
		claude = lastClaudePane(e.ws.Panes)
	}
	m.mu.RUnlock()
	if e == nil {
		return fmt.Errorf("unknown workspace %s", wsID)
	}
	deliver := map[string]string{}
	if claude != "" {
		deliver[claude] = cmd
	}
	_, reopened, err := m.reviveIfClosed(wsID, deliver)
	if err != nil {
		return err
	}
	if reopened && claude != "" {
		return nil
	}
	return m.SpawnEphemeralPane(wsID, "", cmd, "claude-peers")
}

// lastClaudePane is the newest pane a Claude ran in: one started with the
// claude harness, or one the bus started for a teammate.
func lastClaudePane(panes []*model.Pane) string {
	for i := len(panes) - 1; i >= 0; i-- {
		if p := panes[i]; p.Harness == "claude" || p.CreatedBy == "claude-peers" {
			return p.ID
		}
	}
	return ""
}
