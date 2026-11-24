package agent

import "testing"

// The runner's code hash is stable within a build and covers the files a
// runner runs.
func TestClaudeServeCodeHash(t *testing.T) {
	a, b := ClaudeServeCodeHash(), ClaudeServeCodeHash()
	if a == "" || a != b || len(a) != 16 {
		t.Fatalf("hash %q, again %q", a, b)
	}
}
