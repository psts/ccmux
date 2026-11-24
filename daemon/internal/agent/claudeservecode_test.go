package agent

import (
	"testing"
	"testing/fstest"
)

// The runner's code hash is stable within a build.
func TestClaudeServeCodeHash(t *testing.T) {
	a, b := ClaudeServeCodeHash(), ClaudeServeCodeHash()
	if a == "" || a != b || len(a) != 16 {
		t.Fatalf("hash %q, again %q", a, b)
	}
}

// It follows the files: one byte of one file, a file's name, or a file
// gone each change it.
func TestCodeHashFollowsTheFiles(t *testing.T) {
	names := []string{"agent.mjs", "serve.mjs"}
	base := fstest.MapFS{"agent.mjs": {Data: []byte("a")}, "serve.mjs": {Data: []byte("s")}}
	h := codeHashOf(base, names)
	if codeHashOf(fstest.MapFS{"agent.mjs": {Data: []byte("a")}, "serve.mjs": {Data: []byte("s")}}, names) != h {
		t.Fatal("the same files hash differently")
	}
	for name, other := range map[string]string{
		"a byte":   codeHashOf(fstest.MapFS{"agent.mjs": {Data: []byte("b")}, "serve.mjs": {Data: []byte("s")}}, names),
		"a name":   codeHashOf(fstest.MapFS{"agent2.mjs": {Data: []byte("a")}, "serve.mjs": {Data: []byte("s")}}, []string{"agent2.mjs", "serve.mjs"}),
		"one gone": codeHashOf(fstest.MapFS{"serve.mjs": {Data: []byte("s")}}, names),
	} {
		if other == h {
			t.Errorf("%s changed, the hash did not", name)
		}
	}
}
