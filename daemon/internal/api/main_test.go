package api

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"ccmux.dev/ccmuxd/internal/hooktrace"
	"ccmux.dev/ccmuxd/internal/tmux/tmuxtest"
)

// TestMain redirects the hook trace for every test in this package.
//
// hooktrace defaults to ~/Library/Logs/ccmux-hooks.jsonl — the file a developer
// tails to debug their own notifications. Without this, running the suite
// injects fabricated push decisions ("suppressed alice@example.com") into that
// log and makes it lie about what the daemon did. Its tmux servers are its
// own too (tmuxtest), so a second test run on the host cannot kill them.
func TestMain(m *testing.M) {
	dropStaleTempDirs()
	freeServers := tmuxtest.PrivateServers()
	dir, err := os.MkdirTemp("", "ccmux-api-trace")
	if err != nil {
		panic(err)
	}
	hooktrace.SetPath(filepath.Join(dir, "trace.jsonl"))
	code := m.Run()
	_ = os.RemoveAll(dir)
	freeServers()
	// The ccmuxd a test built for its panes (envExecBinary) goes with the
	// run: left behind, each run kept 45 MB in the temp folder, and they
	// filled the disk (78 of them by 2026-11-17).
	if envExecPath != "" {
		_ = os.RemoveAll(filepath.Dir(envExecPath))
	}
	os.Exit(code)
}

// dropStaleTempDirs removes what earlier runs of this package left in the
// temp folder when they were cut short (a timeout or a kill skips the
// cleanup at the end of TestMain): 33 builds of 45 MB each were there on
// 2026-11-23. Only folders older than six hours go, so a run going on beside
// this one keeps its own.
func dropStaleTempDirs() {
	cutoff := time.Now().Add(-6 * time.Hour)
	for _, pattern := range []string{"ccmuxd-envexec-*", "ccmux-api-trace*"} {
		dirs, _ := filepath.Glob(filepath.Join(os.TempDir(), pattern))
		for _, d := range dirs {
			if st, err := os.Stat(d); err == nil && st.IsDir() && st.ModTime().Before(cutoff) {
				_ = os.RemoveAll(d)
			}
		}
	}
}
