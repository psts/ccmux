package manager

import "ccmux.dev/ccmuxd/internal/model"

// MembershipChange is one workspace entering or leaving a shared window,
// for the bus to tell the window's other sessions: a repo or agent that
// joined is someone to hand work to, one that left is not. From and To are
// window names, "" for none; a move between windows has both. A restart
// or a close changes no membership, so it is never one of these.
type MembershipChange struct {
	WorkspaceID string
	Name        string
	RepoPath    string
	Agent       string // the base agent's name for an agent instance, "" for a repo
	From, To    string
}

func changeOf(ws *model.Workspace) MembershipChange {
	return MembershipChange{WorkspaceID: ws.ID, Name: ws.Name, RepoPath: ws.RepoPath, Agent: ws.Agent}
}

// membershipChanged reports a workspace's window after an assignment,
// against the window it was in before; the same window is no change.
func (m *Manager) membershipChanged(wsID, from string) {
	to := m.windowSnapshot().resolve(wsID, "")
	if to == from || m.OnMembershipChange == nil {
		return
	}
	m.mu.RLock()
	e := m.byID[wsID]
	var c MembershipChange
	if e != nil {
		c = changeOf(e.ws)
	}
	m.mu.RUnlock()
	if e == nil {
		return
	}
	c.From, c.To = from, to
	m.OnMembershipChange(c)
}

// membershipLeft reports a deleted workspace leaving its window, c.From,
// which the delete read before removing the membership row.
func (m *Manager) membershipLeft(c MembershipChange) {
	if m.OnMembershipChange == nil || c.From == "" {
		return
	}
	m.OnMembershipChange(c)
}
