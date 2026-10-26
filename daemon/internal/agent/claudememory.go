package agent

import (
	"os"
	"path/filepath"
	"regexp"
)

// notProjectChar is what Claude Code turns into "-" when it names a folder's
// project directory under ~/.claude/projects: every character that is not
// an ASCII letter or digit ("/home/u/.ccmux/a" → "-home-u--ccmux-a").
var notProjectChar = regexp.MustCompile(`[^A-Za-z0-9]`)

// ClaudeMemoryDir is the auto-memory store Claude Code keeps for a session
// started in cwd, <config>/projects/<cwd with every other character a
// dash>/memory, where config is $CLAUDE_CONFIG_DIR or ~/.claude. Its
// MEMORY.md is what the session loads at start, so a walled agent needs it
// writable to keep or correct its memory.
func ClaudeMemoryDir(cwd string) (string, error) {
	config := os.Getenv("CLAUDE_CONFIG_DIR")
	if config == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		config = filepath.Join(home, ".claude")
	}
	return filepath.Join(config, "projects", notProjectChar.ReplaceAllString(cwd, "-"), "memory"), nil
}
