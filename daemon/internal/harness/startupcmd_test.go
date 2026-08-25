package harness

import "testing"

// TestStartupProgram covers the wrappers a startup command can put in front of
// the program. The `env -u TMUX` rows are the ones that regressed: callers
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
		if got := StartupProgram(tc.cmd); got != tc.want {
			t.Errorf("StartupProgram(%q) = %q, want %q", tc.cmd, got, tc.want)
		}
	}
}
