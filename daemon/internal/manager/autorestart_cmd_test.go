package manager

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestResumeCommand(t *testing.T) {
	base := "claude --dangerously-load-development-channels server:claude-peers"
	id := "3e067cdf-2528-4c9d-8832-e099badf7b02"
	cases := []struct {
		name, session, want string
		ok                  bool
	}{
		{"every relaunch resumes", id, base + " --resume " + id, true},
		{"no session refuses", "", "", false},
		{"not a plain id is never typed into a shell", id + "; rm -rf ~", "", false},
	}
	for _, c := range cases {
		if got, ok := resumeCommand(base, c.session); got != c.want || ok != c.ok {
			t.Errorf("%s: got %q,%v want %q,%v", c.name, got, ok, c.want, c.ok)
		}
	}
}

// transcriptWrite finds a session's transcript in any project folder and
// reads its last write; anything else reads as none.
func TestTranscriptWrite(t *testing.T) {
	dir := t.TempDir()
	id := "3e067cdf-2528-4c9d-8832-e099badf7b02"
	f := filepath.Join(dir, "projects", "-ext-projects-admin", id+".jsonl")
	if err := os.MkdirAll(filepath.Dir(f), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(f, []byte("{}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	when := time.Now().Add(-3 * time.Hour).Truncate(time.Second)
	if err := os.Chtimes(f, when, when); err != nil {
		t.Fatal(err)
	}
	if got := transcriptWrite(dir, id); !got.Equal(when) {
		t.Fatalf("found transcript: %v want %v", got, when)
	}
	for _, other := range []string{"11111111-2222-3333-4444-555555555555", "", id + "*", "../" + id} {
		if got := transcriptWrite(dir, other); !got.IsZero() {
			t.Errorf("%q read as a transcript (%v)", other, got)
		}
	}
}

func TestRelaunchable(t *testing.T) {
	cases := []struct {
		cmd  string
		want bool
	}{
		{"claude --dangerously-load-development-channels server:claude-peers", true},
		{"env -u TMUX claude --dangerously-load-development-channels server:claude-peers", true},
		{"", false},
		{"pnpm dev", false},
		{"claude --resume abc", false},
		{"claude --continue", false},
		{"claude -c", false},
		{"claude --dangerously-load-development-channels server:claude-peers -- do the thing", false},
		{"claude -p hi", false},
	}
	for _, c := range cases {
		if got := relaunchable(c.cmd); got != c.want {
			t.Errorf("%q: got %v want %v", c.cmd, got, c.want)
		}
	}
}

func TestRelaunchFlags(t *testing.T) {
	startup := "claude --dangerously-load-development-channels server:claude-peers"
	cases := []struct {
		name    string
		startup string
		argv    []string
		extra   string
		ok      bool
	}{
		{"as started", startup, []string{"claude", "--dangerously-load-development-channels", "server:claude-peers"}, "", true},
		{"after Claude's own /restart: put the channels back, keep its flags", startup,
			[]string{"/home/u/.local/bin/claude", "--resume", "3e06", "--effort", "high", "--permission-mode", "auto"},
			"--effort high --permission-mode auto", true},
		{"a model picked by hand carries over", startup,
			[]string{"claude", "--dangerously-load-development-channels", "server:claude-peers", "--model", "haiku"}, "--model haiku", true},
		{"a flag the startup command lacks", startup,
			[]string{"claude", "--dangerously-load-development-channels", "server:claude-peers", "--add-dir", "/x"}, "", false},
		{"a carried flag with another value than the startup's", "claude --model opus", []string{"claude", "--model", "haiku"}, "", false},
		{"a startup flag the human left off is not added back", "claude --dangerously-skip-permissions",
			[]string{"claude"}, "", false},
		{"a value that would glob is quoted", startup,
			[]string{"claude", "--model", "claude-opus-5-5[1m]"}, "--model 'claude-opus-5-5[1m]'", true},
		{"a value holding a quote is refused", startup,
			[]string{"claude", "--model", "x'; rm -rf ~; '"}, "", false},
		{"a carried flag with no value", startup, []string{"claude", "--model"}, "", false},
		{"no argv read", startup, nil, "", true},
		{"env prefix in the startup command", "env -u TMUX claude --verbose", []string{"claude", "--verbose"}, "", true},
	}
	for _, c := range cases {
		extra, ok := relaunchFlags(c.startup, c.argv)
		if ok != c.ok || strings.Join(extra, " ") != c.extra {
			t.Errorf("%s: got %q,%v want %q,%v", c.name, strings.Join(extra, " "), ok, c.extra, c.ok)
		}
	}
}
