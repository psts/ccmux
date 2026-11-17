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
	// The ccmuxd a test built for its panes (envExecBinary) goes with the
	// run: left behind, each run kept 45 MB in the temp folder, and they
	// filled the disk (78 of them by 2026-11-17).
	if envExecPath != "" {
		_ = os.RemoveAll(filepath.Dir(envExecPath))
	}
	os.Exit(code)
}
