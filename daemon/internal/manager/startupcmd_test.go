package manager

import (
	"testing"

	"ccmux.dev/ccmuxd/internal/harness"
)

// TestStartupProgram covers the wrappers a startup command can put in front of
// the program. The `env -u TMUX` rows are the ones that regressed: both callers
// used to read the wrapper as the program, so a hosted Claude pane came up
// titled "Terminal".
func TestStartupProgram(t *testing.T) {
	cases := []struct{ cmd, want string }{
		{"", ""},
		{"claude", "claude"},
		{"claude --continue", "claude"},
		{"env -u TMUX claude --dangerously-load-development-channels server:claude-peers", "claude"},
		{"env -u TMUX -u FOO claude", "claude"},
		{"env --unset TMUX claude", "claude"},
		{"env --unset=TMUX claude", "claude"},
		{"env -i claude", "claude"},
		{"env TMUX= claude", "claude"},
		{"/usr/bin/env -u TMUX /usr/local/bin/claude", "claude"},
		{"FOO=bar claude", "claude"},
		{"FOO=bar env -u TMUX claude", "claude"},
		{"npm run dev", "npm"},
		{"env -u TMUX npm run dev", "npm"},
		{"env", ""},
		{"env -u TMUX", ""},
	}
	for _, tc := range cases {
		if got := startupProgram(tc.cmd); got != tc.want {
			t.Errorf("startupProgram(%q) = %q, want %q", tc.cmd, got, tc.want)
		}
	}
}

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
