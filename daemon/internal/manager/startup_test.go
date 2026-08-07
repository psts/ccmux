package manager

import (
	"strings"
	"testing"
)

// TestFallbackStartupCommand_HidesTMUX pins the one token that makes copies
// reach the lens. Claude Code picks tmux-buffer over OSC 52 whenever $TMUX is
// set, and a tmux buffer lives on the daemon's host where no lens can see it.
// Without `env -u TMUX` the copy silently goes nowhere — the failure this whole
// path exists to remove, and one nothing else in the daemon would catch.
func TestFallbackStartupCommand_HidesTMUX(t *testing.T) {
	if !strings.HasPrefix(FallbackStartupCommand, "env -u TMUX ") {
		t.Errorf("startup command = %q, want it to hide $TMUX from claude", FallbackStartupCommand)
	}
	if !strings.Contains(FallbackStartupCommand, "server:claude-peers") {
		t.Errorf("startup command lost the peers channel: %q", FallbackStartupCommand)
	}
}
