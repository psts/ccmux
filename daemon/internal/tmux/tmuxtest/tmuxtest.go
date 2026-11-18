// Package tmuxtest gives a test binary tmux servers, and socket paths, of
// its own.
//
// The integration tests name their tmux servers with fixed names
// ("ccmux-api-itest" and the like) and kill that server before they start.
// tmux keeps a named server's socket under $TMUX_TMPDIR (else /tmp), so two
// test runs on one host, say a task's merge check and a person's gate in
// another checkout, killed each other's sessions: tests failed with "pane
// never reported a bare shell" or "workspace not live", different ones each
// run (task #167's merge check, 2026-11-18).
package tmuxtest

import (
	"os"
	"path/filepath"
)

// PrivateServers points TMUX_TMPDIR at a new folder for this process, so
// every tmux server it and its children start is its own, and returns the
// function that removes the folder. Call it first in TestMain. The folder
// is short on purpose: a Unix socket path has a length limit.
func PrivateServers() func() {
	dir, err := os.MkdirTemp("", "ccmux-tmux")
	if err != nil {
		panic(err)
	}
	if err := os.Setenv("TMUX_TMPDIR", dir); err != nil {
		panic(err)
	}
	return func() { _ = os.RemoveAll(dir) }
}

// Socket is a short path for a Unix socket a test binds, in this process's
// own folder from PrivateServers, so two test runs do not fight over one
// fixed path in /tmp. It falls back to os.TempDir before PrivateServers.
func Socket(name string) string {
	dir := os.Getenv("TMUX_TMPDIR")
	if dir == "" {
		dir = os.TempDir()
	}
	return filepath.Join(dir, name)
}
