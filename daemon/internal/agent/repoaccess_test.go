package agent

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// gitRepo makes a repo whose .gitignore lists the local settings when ignored.
func gitRepo(t *testing.T, ignored bool) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	// This host's own git ignores may list the file in every repo; only the
	// repo's .gitignore may decide here.
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	dir := t.TempDir()
	if out, err := exec.Command("git", "-C", dir, "init", "-q").CombinedOutput(); err != nil {
		t.Fatalf("git init: %v %s", err, out)
	}
	ignore := "node_modules\n"
	if ignored {
		ignore += ".claude/settings.local.json\n"
	}
	if err := os.WriteFile(filepath.Join(dir, ".gitignore"), []byte(ignore), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestGrantRepoDir_AddsOnceAndKeepsTheRest(t *testing.T) {
	repo := gitRepo(t, true)
	path := filepath.Join(repo, ".claude", "settings.local.json")
	os.MkdirAll(filepath.Dir(path), 0o755)
	os.WriteFile(path, []byte(`{"permissions":{"allow":["Bash(npm test)"],"additionalDirectories":["/other"]},"model":"opus"}`), 0o600)

	if wrote, err := GrantRepoDir(repo, "/win/shared/app"); err != nil || !wrote {
		t.Fatalf("first grant = %v %v", wrote, err)
	}
	if wrote, err := GrantRepoDir(repo, "/win/shared/app"); err != nil || wrote {
		t.Fatalf("second grant = %v %v, want no write", wrote, err)
	}
	raw, _ := os.ReadFile(path)
	var got struct {
		Permissions struct {
			Allow []string `json:"allow"`
			Dirs  []string `json:"additionalDirectories"`
		} `json:"permissions"`
		Model string `json:"model"`
	}
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	if strings.Join(got.Permissions.Dirs, ",") != "/other,/win/shared/app" || got.Model != "opus" || len(got.Permissions.Allow) != 1 {
		t.Fatalf("settings after grant = %s", raw)
	}
	if fi, _ := os.Stat(path); fi.Mode().Perm() != 0o600 {
		t.Errorf("mode = %v, want the file's own 0600 kept", fi.Mode().Perm())
	}
}

func TestGrantRepoDir_LeavesWhatItMustNotTouch(t *testing.T) {
	plain := gitRepo(t, false)
	if wrote, err := GrantRepoDir(plain, "/win/shared/app"); err == nil || wrote {
		t.Fatalf("not-ignored repo = %v %v, want a refusal", wrote, err)
	}
	if _, err := os.Stat(filepath.Join(plain, ".claude")); err == nil {
		t.Error("a repo whose git does not ignore the file got one")
	}
	notRepo := t.TempDir()
	if wrote, err := GrantRepoDir(notRepo, "/win/shared/app"); wrote || err == nil ||
		strings.Contains(err.Error(), "does not ignore") || !strings.Contains(err.Error(), "not a git repository") {
		t.Fatalf("not a repo = %v %v, want git's own words, not \"does not ignore\"", wrote, err)
	}
	broken := gitRepo(t, true)
	path := filepath.Join(broken, ".claude", "settings.local.json")
	os.MkdirAll(filepath.Dir(path), 0o755)
	os.WriteFile(path, []byte(`{"permissions": `), 0o644)
	if wrote, err := GrantRepoDir(broken, "/win/shared/app"); err == nil || wrote {
		t.Fatalf("unparseable file = %v %v, want a refusal", wrote, err)
	}
	if raw, _ := os.ReadFile(path); string(raw) != `{"permissions": ` {
		t.Errorf("unparseable file rewritten: %s", raw)
	}
}

func TestGrantRepoDir_CreatesTheFileWhenIgnored(t *testing.T) {
	repo := gitRepo(t, true)
	if wrote, err := GrantRepoDir(repo, "/win/shared/app"); err != nil || !wrote {
		t.Fatalf("grant = %v %v", wrote, err)
	}
	raw, err := os.ReadFile(filepath.Join(repo, ".claude", "settings.local.json"))
	if err != nil || !strings.Contains(string(raw), "/win/shared/app") {
		t.Fatalf("new file = %s %v", raw, err)
	}
}
