package main

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
)

// specScripts reads the sidecar each session spec names, and nothing from
// the runners' own state files beside them.
func TestSpecScriptsReadsTheSpecsOnly(t *testing.T) {
	state := t.TempDir()
	t.Setenv("XDG_STATE_HOME", state)
	t.Setenv("CCMUXD_ROOT", "")
	dir := runnerStateDir()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	files := map[string]string{
		"s1.json":       `{"node":"/bin/node","script":"/a/claude-sdk/0.3.330/serve.mjs","dir":"/r"}`,
		"s1.state.json": `{"open":[],"delivered":[]}`,
		"s2.json":       `{"node":"/bin/node","script":"/a/claude-serve/serve.mjs","dir":"/r"}`,
		"broken.json":   `not json`,
	}
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	got := specScripts()
	slices.Sort(got)
	if want := []string{"/a/claude-sdk/0.3.330/serve.mjs", "/a/claude-serve/serve.mjs"}; !slices.Equal(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}
