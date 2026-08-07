package manager

import (
	"strings"
	"testing"
)

// TestFallbackStartupCommand_HidesTMUX pins the `env -u TMUX` prefix. It does
// NOT pin "copies reach the lens": measured against Claude Code 2.1.224, they
// reach it with $TMUX set too, because it emits OSC 52 on every copy. What the
// prefix avoids is a duplicate sequence and an up-to-4s blocking
// `tmux load-buffer` ahead of each copy.
//
// Note the narrow scope: this guards only the FALLBACK. An install with
// default_startup_command persisted, or a matching startup rule, never reaches
// this constant at all — see Manager.DefaultStartupCommand.
func TestFallbackStartupCommand_HidesTMUX(t *testing.T) {
	if !strings.HasPrefix(FallbackStartupCommand, "env -u TMUX ") {
		t.Errorf("startup command = %q, want it to hide $TMUX from claude", FallbackStartupCommand)
	}
	if !strings.Contains(FallbackStartupCommand, "server:claude-peers") {
		t.Errorf("startup command lost the peers channel: %q", FallbackStartupCommand)
	}
}
