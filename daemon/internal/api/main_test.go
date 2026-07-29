package api

import (
	"os"
	"path/filepath"
	"testing"

	"ccmux.dev/ccmuxd/internal/hooktrace"
)

// TestMain redirects the hook trace for every test in this package.
//
// hooktrace defaults to ~/Library/Logs/ccmux-hooks.jsonl — the file a developer
// tails to debug their own notifications. Without this, running the suite
// injects fabricated push decisions ("suppressed alice@example.com") into that
// log and makes it lie about what the daemon did.
func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "ccmux-api-trace")
	if err != nil {
		panic(err)
	}
	hooktrace.SetPath(filepath.Join(dir, "trace.jsonl"))
	code := m.Run()
	_ = os.RemoveAll(dir)
	os.Exit(code)
}
