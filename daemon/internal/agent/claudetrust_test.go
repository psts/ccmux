package agent

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func readProjects(t *testing.T, path string) map[string]any {
	t.Helper()
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var cfg map[string]any
	if err := json.Unmarshal(body, &cfg); err != nil {
		t.Fatalf("config after write does not parse: %v\n%s", err, body)
	}
	return cfg
}

func flagsSet(project map[string]any) bool {
	for _, f := range claudeProjectFlags {
		if v, _ := project[f].(bool); !v {
			return false
		}
	}
	return true
}

// The three flags land for the instance folder and nothing else in the
// file moves: the user's other keys, other projects, and the project's own
// other keys all survive.
func TestApproveClaudeProject_FlagsLandOthersSurvive(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".claude.json")
	seed := `{"numStartups":42,"mcpServers":{"peers":{"command":"ccmux-peers"}},"projects":{"/home/u/other":{"hasTrustDialogAccepted":false,"allowedTools":["Bash"]},"/home/u/agents/x-post":{"lastCost":1.5}}}`
	if err := os.WriteFile(path, []byte(seed), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := ApproveClaudeProject(path, "/home/u/agents/x-post"); err != nil {
		t.Fatal(err)
	}
	cfg := readProjects(t, path)
	if cfg["numStartups"] != 42.0 || cfg["mcpServers"].(map[string]any)["peers"] == nil {
		t.Errorf("top-level keys must survive: %v", cfg)
	}
	projects := cfg["projects"].(map[string]any)
	mine := projects["/home/u/agents/x-post"].(map[string]any)
	if !flagsSet(mine) || mine["lastCost"] != 1.5 {
		t.Errorf("instance project: flags must land beside its other keys: %v", mine)
	}
	other := projects["/home/u/other"].(map[string]any)
	if other["hasTrustDialogAccepted"] != false || len(other["allowedTools"].([]any)) != 1 {
		t.Errorf("another project must not be touched: %v", other)
	}
	if info, _ := os.Stat(path); info.Mode().Perm() != 0o600 {
		t.Errorf("the config holds account state; mode %v, want 0600", info.Mode().Perm())
	}
}

// No file yet (a host that never ran the TUI): a minimal one is created.
// Already approved: the file is not rewritten at all.
func TestApproveClaudeProject_MissingAndIdempotent(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".claude.json")
	if err := ApproveClaudeProject(path, "/a/b"); err != nil {
		t.Fatal(err)
	}
	cfg := readProjects(t, path)
	if !flagsSet(cfg["projects"].(map[string]any)["/a/b"].(map[string]any)) {
		t.Errorf("fresh file: %v", cfg)
	}
	before, _ := os.Stat(path)
	if err := os.Chtimes(path, before.ModTime(), before.ModTime().Add(-1e9)); err != nil {
		t.Fatal(err)
	}
	stamped, _ := os.Stat(path)
	if err := ApproveClaudeProject(path, "/a/b"); err != nil {
		t.Fatal(err)
	}
	if after, _ := os.Stat(path); !after.ModTime().Equal(stamped.ModTime()) {
		t.Error("already approved: the file must not be rewritten")
	}
}

// A file that does not parse is Claude Code's whole per-user state: it is
// reported, never overwritten with a fresh one.
func TestApproveClaudeProject_UnparseableUntouched(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".claude.json")
	for _, body := range []string{`{"projects":`, `[]`, `null`} {
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		err := ApproveClaudeProject(path, "/a/b")
		if err == nil || !strings.Contains(err.Error(), "untouched") {
			t.Errorf("%q: want a parse error, got %v", body, err)
		}
		if got, _ := os.ReadFile(path); string(got) != body {
			t.Errorf("%q: file was rewritten to %q", body, got)
		}
	}
	if left, _ := filepath.Glob(filepath.Join(filepath.Dir(path), ".claude.json.*")); len(left) != 0 {
		t.Errorf("temp files left behind: %v", left)
	}
}

// Several instances starting at once each add their own folder: none may
// rename over another's flags, and a folder that was already there keeps
// its own untouched entry.
func TestApproveClaudeProject_ConcurrentStarts(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".claude.json")
	seed := `{"projects":{"/home/u/other":{"hasTrustDialogAccepted":false,"allowedTools":["Bash"]}}}`
	if err := os.WriteFile(path, []byte(seed), 0o600); err != nil {
		t.Fatal(err)
	}
	const n = 50
	var wg sync.WaitGroup
	errs := make(chan error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			if err := ApproveClaudeProject(path, fmt.Sprintf("/d/%d", i)); err != nil {
				errs <- err
			}
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
	projects := readProjects(t, path)["projects"].(map[string]any)
	for i := 0; i < n; i++ {
		p, _ := projects[fmt.Sprintf("/d/%d", i)].(map[string]any)
		if !flagsSet(p) {
			t.Errorf("/d/%d lost its flags to a concurrent start: %v", i, p)
		}
	}
	other := projects["/home/u/other"].(map[string]any)
	if other["hasTrustDialogAccepted"] != false || len(other["allowedTools"].([]any)) != 1 {
		t.Errorf("the untouched project changed: %v", other)
	}
}
