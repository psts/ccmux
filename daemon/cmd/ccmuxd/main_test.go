package main

import (
	"os"
	"testing"

	"ccmux.dev/ccmuxd/internal/tmux/tmuxtest"
)

// TestMain gives this package's tests tmux servers of their own (tmuxtest):
// the loadout and runner wiring tests name theirs with fixed names, so a
// second test run on the host could kill them mid-test.
func TestMain(m *testing.M) {
	freeServers := tmuxtest.PrivateServers()
	code := m.Run()
	freeServers()
	os.Exit(code)
}
