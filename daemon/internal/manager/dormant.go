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
// session. Only such panes can go dormant — a pane opened as a plain terminal
// has no session to lose.
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

// isDormant decides from the two facts the pane already carries: it was started
// to host Claude, and its foreground command is now a bare shell.
func isDormant(p *model.Pane) bool {
	if p.DevServer || !startsClaude(p.StartupCommand) {
		return false
	}
	return shellCommands[p.RawCommand]
}

// refreshDormantLocked recomputes a pane's dormant flag, returning whether it
// changed so the caller can persist and broadcast exactly once.
func refreshDormantLocked(p *model.Pane) bool {
	was := p.Dormant
	p.Dormant = isDormant(p)
	return p.Dormant != was
}
