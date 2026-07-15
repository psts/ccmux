package gitstatus

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestParse_BranchHeadersAndFiles(t *testing.T) {
	out := strings.Join([]string{
		"# branch.oid 1234567890abcdef",
		"# branch.head lens-pivot",
		"# branch.upstream origin/lens-pivot",
		"# branch.ab +2 -1",
		"1 M. N... 100644 100644 100644 abc def Sources/App.swift",
		"1 .M N... 100644 100644 100644 abc def daemon/web/app.js",
		"1 MM N... 100644 100644 100644 abc def both changed.txt",
		"1 A. N... 000000 100644 100644 000 def new-staged.go",
		"1 .D N... 100644 100644 000000 abc 000 gone.txt",
		"2 R. N... 100644 100644 100644 abc def R100 new-name.txt\told-name.txt",
		"? scratch.log",
	}, "\n")

	st := Parse(out)
	if st.Branch != "lens-pivot" || st.TrackingBranch != "origin/lens-pivot" {
		t.Errorf("branch/tracking = %q/%q", st.Branch, st.TrackingBranch)
	}
	if st.Ahead != 2 || st.Behind != 1 {
		t.Errorf("ahead/behind = %d/%d, want 2/1", st.Ahead, st.Behind)
	}
	staged := paths(st.StagedFiles)
	if !equal(staged, []string{"Sources/App.swift", "both changed.txt", "new-staged.go", "new-name.txt"}) {
		t.Errorf("staged = %v", staged)
	}
	if !equal(paths(st.ModifiedFiles), []string{"daemon/web/app.js", "both changed.txt"}) {
		t.Errorf("modified = %v", paths(st.ModifiedFiles))
	}
	if !equal(paths(st.DeletedFiles), []string{"gone.txt"}) {
		t.Errorf("deleted = %v", paths(st.DeletedFiles))
	}
	if !equal(paths(st.UntrackedFiles), []string{"scratch.log"}) {
		t.Errorf("untracked = %v", paths(st.UntrackedFiles))
	}
	if st.StagedFiles[3].Status != "R" || st.UntrackedFiles[0].Status != "?" {
		t.Errorf("statuses = %+v / %+v", st.StagedFiles[3], st.UntrackedFiles[0])
	}
}

func TestParse_DetachedHeadUsesShortSHA(t *testing.T) {
	out := "# branch.oid 1234567890abcdef\n# branch.head (detached)\n"
	if st := Parse(out); st.Branch != "1234567" {
		t.Errorf("branch = %q, want short sha", st.Branch)
	}
}

func TestParse_Empty(t *testing.T) {
	st := Parse("")
	if st.Branch != "" || len(st.StagedFiles)+len(st.ModifiedFiles)+len(st.UntrackedFiles)+len(st.DeletedFiles) != 0 {
		t.Errorf("empty parse = %+v", st)
	}
}

// TestFull_RealRepo exercises the whole pipeline against a real throwaway git
// repo: branch, staged/modified/untracked buckets, default-branch detection and
// the vs-default ahead count.
func TestFull_RealRepo(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	ctx := context.Background()
	repo := t.TempDir()
	git := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = repo
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	write := func(name, content string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(repo, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	git("init", "-b", "main")
	write("a.txt", "one")
	git("add", "a.txt")
	git("commit", "-m", "base")
	git("checkout", "-b", "feature")
	write("a.txt", "two")
	git("commit", "-am", "ahead-1")
	write("a.txt", "three")   // modified, unstaged
	write("new.txt", "fresh") // untracked

	if db := DetectDefaultBranch(ctx, repo); db != "main" {
		t.Fatalf("default branch = %q, want main", db)
	}
	st, err := Full(ctx, repo, "main")
	if err != nil {
		t.Fatalf("Full: %v", err)
	}
	if !st.IsGitRepo || st.Branch != "feature" {
		t.Errorf("repo/branch = %v/%q", st.IsGitRepo, st.Branch)
	}
	if st.DefaultBranch != "main" || st.AheadOfDefault != 1 || st.BehindDefault != 0 {
		t.Errorf("vs default = %q ↑%d ↓%d, want main ↑1 ↓0", st.DefaultBranch, st.AheadOfDefault, st.BehindDefault)
	}
	if !equal(paths(st.ModifiedFiles), []string{"a.txt"}) || !equal(paths(st.UntrackedFiles), []string{"new.txt"}) {
		t.Errorf("files = mod %v untracked %v", paths(st.ModifiedFiles), paths(st.UntrackedFiles))
	}
}

func TestFull_NotARepo(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	st, err := Full(context.Background(), t.TempDir(), "")
	if err != nil {
		t.Fatalf("Full: %v", err)
	}
	if st.IsGitRepo {
		t.Error("plain temp dir reported as a repo")
	}
}

func paths(files []File) []string {
	out := make([]string, len(files))
	for i, f := range files {
		out[i] = f.Path
	}
	return out
}

func equal(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
