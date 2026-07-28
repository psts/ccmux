// Automatic pane titles: lenses label tabs with what a pane actually runs
// ("Claude", "Terminal", "vim", "dev ▸ npm run dev") instead of "pane N".
// tmux is the source: a control-mode format subscription pushes every pane's
// #{pane_title} and #{pane_current_command} into the session controller, which
// surfaces them as notices; this file turns those signals into a title.
package manager

import (
	"os"
	"regexp"
	"strings"

	"ccmux.dev/ccmuxd/internal/model"
)

// shellNames are foreground commands that mean "nothing is running" — the pane
// is just a shell, so it's a Terminal regardless of any stale OSC title left
// behind by a program that exited (titles are never reset automatically).
var shellNames = map[string]bool{
	"zsh": true, "bash": true, "fish": true, "sh": true,
	"tcsh": true, "ksh": true, "dash": true, "nu": true, "login": true,
}

// versionish matches Claude Code's process name: its argv[0] is the bare
// version string (e.g. "2.1.211" — verified live). No other CLI is named a
// dotted number, so this identifies claude before its OSC title arrives.
var versionish = regexp.MustCompile(`^[0-9]+(\.[0-9]+)+$`)

// derivePaneTitle turns the tmux runtime signals into a lens-facing title.
// Empty means "no verdict — keep the current title". defaultTitles holds the
// values #{pane_title} shows when no program set one (the tmux host's name).
func derivePaneTitle(rawTitle, rawCommand string, defaultTitles map[string]bool) string {
	cmd := strings.TrimSpace(rawCommand)
	if cmd == "" {
		return ""
	}
	if shellNames[strings.TrimPrefix(cmd, "-")] {
		return "Terminal"
	}
	if t := strings.TrimSpace(rawTitle); t != "" && !defaultTitles[t] {
		if strings.Contains(t, "Claude Code") {
			return "Claude"
		}
		return t
	}
	if versionish.MatchString(cmd) {
		return "Claude"
	}
	return cmd
}

// initialPaneTitle is the spawn-time guess shown until the first tmux signal
// arrives (~1s): panes born to run claude are "Claude", everything else starts
// as a shell.
func initialPaneTitle(startupCmd string) string {
	if f := strings.Fields(startupCmd); len(f) > 0 && f[0] == "claude" {
		return "Claude"
	}
	return "Terminal"
}

// defaultPaneTitles returns the #{pane_title} values that mean "no program set
// a title": tmux defaults the title to #H (the host's name). The daemon and
// tmux always share a machine, so os.Hostname covers it (full + short form).
func defaultPaneTitles() map[string]bool {
	titles := map[string]bool{}
	if h, err := os.Hostname(); err == nil && h != "" {
		titles[h] = true
		titles[strings.SplitN(h, ".", 2)[0]] = true
	}
	return titles
}

// applyPaneTitleSignal folds one tmux signal (kind "pane-title" or
// "pane-command") into the pane and re-derives its title; a real change
// persists and broadcasts so lenses refresh. The dev-server pane keeps its
// purposeful "dev ▸ …" title.
func (m *Manager) applyPaneTitleSignal(wsID, paneID, kind, value string) {
	m.mu.Lock()
	e := m.byID[wsID]
	if e == nil {
		m.mu.Unlock()
		return
	}
	var p *model.Pane
	for _, q := range e.ws.Panes {
		if q.ID == paneID {
			p = q
			break
		}
	}
	if p == nil || p.DevServer {
		m.mu.Unlock()
		return
	}
	if kind == "pane-title" {
		p.RawTitle = value
	} else {
		p.RawCommand = value
	}
	// The same command signal that renames a pane also reveals a Claude that
	// exited, so dormancy is recomputed here rather than polled for.
	changed := refreshDormantLocked(p)
	shell := atBareShell(p)
	if title := derivePaneTitle(p.RawTitle, p.RawCommand, m.paneTitleDefaults); title != "" && title != p.Title {
		p.Title = title
		changed = true
	}
	saved := *p // copy: p is shared state and the writes below happen unlocked
	m.mu.Unlock()
	// The backstop, asserted on EVERY command signal rather than only on a change.
	// tmux replays each pane's current command on subscribe, so this is what
	// re-establishes the truth after a daemon restart — a moment when nothing has
	// "changed" but everything has been forgotten. It cannot be lost, so it
	// retires sessions no SessionEnd ever reported: a killed Claude, a session
	// predating the hooks, a daemon that was down.
	switch {
	case shell:
		m.ApplySession(paneID, "", model.SessionNone)
	case kind == "pane-command" && !runsClaude(&saved) && !m.paneHasLiveSession(paneID):
		// Not a shell, not Claude, and no session behind it: the pane has been
		// given to other work. Whatever Claude used to live here is history, and
		// history is not what dormancy reports.
		m.clearHostedClaude(paneID)
	}
	if !changed {
		return
	}
	_ = m.store.SavePane(&saved)
	m.events.publish(Event{Kind: "workspace-status", WorkspaceID: wsID})
}
