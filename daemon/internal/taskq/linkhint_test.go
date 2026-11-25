package taskq

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A link a worker left (task #171: apps/api/.venv pointing at the main
// checkout's) is named with how to replace it; a plain file is not.
func TestLinkHint(t *testing.T) {
	wt := t.TempDir()
	os.MkdirAll(filepath.Join(wt, "apps", "api"), 0o755)
	os.Symlink(t.TempDir(), filepath.Join(wt, "apps", "api", ".venv"))
	os.WriteFile(filepath.Join(wt, "notes.txt"), []byte("x"), 0o644)

	got := linkHint(wt, "?? apps/api/.venv, ?? notes.txt")
	if !strings.Contains(got, "apps/api/.venv is a link") || !strings.Contains(got, "uv venv") {
		t.Fatalf("the link is named with its fix: %q", got)
	}
	if strings.Contains(got, "notes.txt") {
		t.Fatalf("a plain file is not called a link: %q", got)
	}
	if got := linkHint(wt, "?? notes.txt"); got != "" {
		t.Fatalf("no link, no hint: %q", got)
	}
	if got := linkHint(wt, " M tracked.go"); got != "" {
		t.Fatalf("a changed tracked file is no link: %q", got)
	}
}

// Every builder is told to make its own Python setup with uv.
func TestServerRulesSayUv(t *testing.T) {
	if !strings.Contains(serverRules, "uv venv") || !strings.Contains(serverRules, "never a link") {
		t.Fatal("the brief must tell builders to make their own venv with uv")
	}
}

// A builder that runs a scratch ccmuxd is told to give it its own three
// unit names: the defaults are the person's live services.
func TestServerRulesNameAScratchDaemonsThreeUnits(t *testing.T) {
	for _, want := range []string{"CCMUXD_ROOT", "-runner-unit-prefix", "-job-unit-prefix", "-llm-proxy-unit", "never the defaults"} {
		if !strings.Contains(serverRules, want) {
			t.Errorf("the brief misses %q", want)
		}
	}
}
