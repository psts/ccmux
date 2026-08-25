package manager

import (
	"testing"

	"ccmux.dev/ccmuxd/internal/harness"
)

// TestInitialPaneTitle_FallbackCommand pins the actual shipped claude
// command, so a future change to harness.FallbackClaudeCommand that hides the
// program again fails here rather than showing up as a mislabelled tab.
func TestInitialPaneTitle_FallbackCommand(t *testing.T) {
	if got := initialPaneTitle(harness.FallbackClaudeCommand); got != "Claude" {
		t.Errorf("initialPaneTitle(FallbackClaudeCommand) = %q, want Claude", got)
	}
	if !startsClaude(harness.FallbackClaudeCommand) {
		t.Error("startsClaude(FallbackClaudeCommand) = false, want true")
	}
}

// guessHarness is the SEED for a pane's harness when it is created by raw
// command: claude-shaped commands are claude panes, everything else is a
// plain shell until a harness spawn says otherwise.
func TestGuessHarness(t *testing.T) {
	cases := map[string]string{
		"claude": "claude",
		"env -u TMUX claude --dangerously-load-development-channels x": "claude",
		"FOO=1 claude --continue":                                      "claude",
		":":                                                            "",
		"":                                                             "",
		"vim":                                                          "",
	}
	for cmd, want := range cases {
		if got := guessHarness(cmd); got != want {
			t.Errorf("guessHarness(%q) = %q, want %q", cmd, got, want)
		}
	}
}
