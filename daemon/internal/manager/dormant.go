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
	"path/filepath"
	"strings"

	"ccmux.dev/ccmuxd/internal/model"
)

// shellCommands are foreground commands that mean "nothing is running here".
// Deliberately just shells: anything else the user started (vim, a REPL, a
// build) is real work and must not be labelled dormant.
var shellCommands = map[string]bool{
	"zsh": true, "bash": true, "sh": true, "fish": true, "dash": true, "ksh": true,
	"-zsh": true, "-bash": true, "-sh": true, "-fish": true,
}

// startsClaude reports whether a startup command was meant to run a Claude
// session. This is only a SEED for hosted_claude on a freshly created pane and
// for the one-time migration of existing ones; it is never the runtime test,
// because it cannot see a Claude the user launched by hand.
func startsClaude(startupCmd string) bool {
	fields := strings.Fields(startupCmd)
	for _, f := range fields {
		// Skip leading env assignments (FOO=bar claude …) and find the program.
		if strings.Contains(f, "=") {
			continue
		}
		return filepath.Base(f) == "claude"
	}
	return false
}

// isDormant is the whole rule, in the two facts a pane carries: a Claude has
// run here, and a bare shell is running here now.
func isDormant(p *model.Pane) bool {
	if p.DevServer || !p.HostedClaude {
		return false
	}
	return shellCommands[p.RawCommand]
}

// atBareShell reports the observation that backstops every missed hook: whatever
// the hooks did or did not say, a pane sitting at a shell is running no session.
func atBareShell(p *model.Pane) bool { return shellCommands[p.RawCommand] }

// refreshDormantLocked recomputes a pane's dormant flag, returning whether it
// changed so the caller can persist and broadcast exactly once.
func refreshDormantLocked(p *model.Pane) bool {
	was := p.Dormant
	p.Dormant = isDormant(p)
	return p.Dormant != was
}
