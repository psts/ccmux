package hooks

import (
	"os"
	"path/filepath"
	"testing"

	"ccmux.dev/ccmuxd/internal/hooktrace"
)

// TestMain redirects the hook trace for every test in this package, so routing a
// fake hook in a test never appears in the real ~/Library/Logs/ccmux-hooks.jsonl
// a developer is reading to explain a real notification.
func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "ccmux-hooks-trace")
	if err != nil {
		panic(err)
	}
	hooktrace.SetPath(filepath.Join(dir, "trace.jsonl"))
	code := m.Run()
	_ = os.RemoveAll(dir)
	os.Exit(code)
}
