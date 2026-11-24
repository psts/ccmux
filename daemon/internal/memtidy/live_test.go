package memtidy

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"ccmux.dev/ccmuxd/internal/store"
)

// TestLiveTidyOnACopy runs a real tidy-up, on a copy of a repo's memory
// and its last day of conversations, never on the live folder. It costs a
// real model run, so it runs only with MEMTIDY_LIVE set to the repo's
// folder (MEMTIDY_MODEL picks the model, sonnet by default); the copy is
// kept in MEMTIDY_OUT when that is set, to read what it did.
func TestLiveTidyOnACopy(t *testing.T) {
	src := os.Getenv("MEMTIDY_LIVE")
	if src == "" {
		t.Skip("set MEMTIDY_LIVE to a repo folder to run a real tidy-up on a copy")
	}
	claude, err := exec.LookPath("claude")
	if err != nil {
		t.Skip("no claude")
	}
	home, _ := os.UserHomeDir()
	realProjects := filepath.Join(home, ".claude", "projects")
	root := os.Getenv("MEMTIDY_OUT")
	if root == "" {
		root = t.TempDir()
	}
	repoPath := filepath.Join(root, "repo")
	os.MkdirAll(filepath.Join(repoPath, ".git"), 0o755)
	projects := filepath.Join(root, "projects")
	notes, err := readNotes(filepath.Join(projectDir(realProjects, src), "memory"))
	if err != nil || len(notes) == 0 {
		t.Fatalf("no memory to copy: %v", err)
	}
	writeNotes(filepath.Join(projectDir(projects, repoPath), "memory"), notes)
	day := time.Now().Add(-24 * time.Hour)
	convs := conversationsOf(realProjects, repo{Path: src, Cwds: []string{src}}, day, time.Now())
	for _, c := range convs {
		b, _ := os.ReadFile(c.Path)
		dst := filepath.Join(projectDir(projects, repoPath), filepath.Base(c.Path))
		os.WriteFile(dst, b, 0o644)
		os.Chtimes(dst, c.MTime, c.MTime)
	}
	model := os.Getenv("MEMTIDY_MODEL")
	if model == "" {
		model = "sonnet"
	}
	tid := &Tidier{
		Sessions: func() ([]store.RunnerSession, error) {
			return []store.RunnerSession{{Kind: store.SessionLead, Cwd: repoPath}}, nil
		},
		Projects: projects, StateDir: filepath.Join(root, "state"),
		Claude: Headless(claude, model, append(os.Environ(), "CLAUDE_CODE_DISABLE_AUTO_MEMORY=1"), 30*time.Minute),
	}
	tid.Once(context.Background())
	for _, o := range readLog(t, tid.StateDir) {
		t.Logf("%s: %d conversations, +%v ~%v -%v %s\n%s", o.Result, o.Conversations, o.Added, o.Changed, o.Removed, o.Why, o.Answer)
	}
}
