// Dormant panes: a pane spawned to run a Claude session whose Claude has
// exited, leaving the shell behind. tmux keeps the pane, so nothing in any lens
// distinguishes it from a working session — the same mistake the peers bus used
// to make one level down, where a live pane was read as a live teammate.
//
// The signal is already flowing: control mode pushes #{pane_current_command} on
// every change (see Controller.subscribeTitles), so the moment Claude exits and
// the shell returns to the foreground, the pane reports "zsh" and we can say so.
package manager

import (
	"strings"

	"ccmux.dev/ccmuxd/internal/harness"
	"ccmux.dev/ccmuxd/internal/model"
)

// startsClaude reports whether a startup command was meant to run a Claude
// session. This is only a SEED for hosted_claude on a freshly created pane and
// for the one-time migration of existing ones; it is never the runtime test,
// because it cannot see a Claude the user launched by hand.
func startsClaude(startupCmd string) bool {
	return harness.StartupProgram(startupCmd) == "claude"
}

// isDormant is the whole rule, in the two facts a pane carries: the last thing
// to run here was a Claude session, and a bare shell is running here now.
func isDormant(p *model.Pane) bool {
	if p.DevServer || !p.HostedClaude {
		return false
	}
	return atBareShell(p)
}

// atBareShell reports the observation that backstops every missed hook: whatever
// the hooks did or did not say, a pane sitting at a shell is running no session.
// Shares shellNames with title derivation so the two can never disagree about
// what counts as "nothing is running here".
func atBareShell(p *model.Pane) bool {
	return shellNames[strings.TrimPrefix(strings.TrimSpace(p.RawCommand), "-")]
}

// runsClaude reports that a pane's foreground IS Claude. It keeps a running
// Claude's pane from being mistaken for one repurposed to other work, even when
// no hook has ever reported its session.
//
// TWO spellings, both seen on this fleet: Claude Code renames its process to the
// bare version ("2.1.211"), which is the signature title derivation keys off —
// but tmux reports a plain "claude" for the same program on the Linux host, and
// before the rename lands anywhere. Matching only the version made a real Claude
// invisible to every rule built on this predicate, which is how a live session
// on that host stayed hidden from the peers bus: nothing recognized it as Claude,
// so nothing ever retracted the pane's stale "at a shell" verdict.
func runsClaude(p *model.Pane) bool { return isClaudeCommand(p.RawCommand) }

// isClaudeCommand is the one place that decides whether a tmux
// #{pane_current_command} names Claude, shared with title derivation so the two
// can never disagree about it — the same reason atBareShell shares shellNames.
func isClaudeCommand(rawCommand string) bool {
	cmd := strings.TrimSpace(rawCommand)
	return versionish.MatchString(cmd) || harness.StartupProgram(cmd) == "claude"
}

// refreshDormantLocked recomputes a pane's dormant and at-shell flags,
// returning whether either changed so the caller can persist and broadcast
// exactly once. AtShell rides along because it derives from the same
// RawCommand signal and the harness pickers key on it.
func refreshDormantLocked(p *model.Pane) bool {
	wasDormant, wasShell := p.Dormant, p.AtShell
	p.Dormant = isDormant(p)
	p.AtShell = atBareShell(p)
	return p.Dormant != wasDormant || p.AtShell != wasShell
}

// PaneAtShell reports whether a pane's foreground is a bare shell right now.
// The peers bus asks this at decision time rather than remembering a past
// signal, so a hook that lands out of order cannot leave a pane believing in a
// session that plainly is not there. Unknown panes answer false: absence of
// knowledge is not evidence of a shell.
func (m *Manager) PaneAtShell(paneID string) bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	_, p := m.findPaneLocked(paneID)
	return p != nil && atBareShell(p)
}

// clearHostedClaude forgets that a Claude ever ran in a pane, called when
// something else takes it over. The flag describes the pane NOW, not its whole
// history: a pane reused for a dev server or an editor is just a pane, and a
// dormancy marker that outlives its truth is noise, which gets ignored.
func (m *Manager) clearHostedClaude(paneID string) {
	m.mu.Lock()
	e, p := m.findPaneLocked(paneID)
	if p == nil || !p.HostedClaude {
		m.mu.Unlock()
		return
	}
	p.HostedClaude = false
	p.Dormant = false
	wsID, saved := e.ws.ID, *p
	m.mu.Unlock()
	_ = m.store.SavePane(&saved)
	m.events.publish(Event{Kind: "workspace-status", WorkspaceID: wsID})
}

// paneHasLiveSession asks the peers bus whether a Claude session is running in
// a pane. Without it a non-shell foreground is ambiguous: Claude working, or the
// pane repurposed. Nil (peers disabled) reads as "no session", which errs toward
// forgetting a stale flag rather than keeping a wrong one.
func (m *Manager) paneHasLiveSession(paneID string) bool {
	if m.SessionLiveFn == nil {
		return false
	}
	return m.SessionLiveFn(paneID)
}
