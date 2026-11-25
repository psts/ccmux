package wtsweep

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
)

// git runs git in dir, under the job's context (it ends at the job's
// limit), and returns what it printed, trimmed. A failure
// carries what git wrote to stderr: that is the reason a person reads.
func git(ctx context.Context, dir string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", append([]string{"-C", dir, "-c", "core.fsmonitor=false"}, args...)...)
	cmd.Env = append(os.Environ(), "LC_ALL=C", "GIT_TERMINAL_PROMPT=0")
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = err.Error()
		}
		return "", fmt.Errorf("git %s: %s", args[0], msg)
	}
	return strings.TrimSpace(string(out)), nil
}

// trunk is the repo's default branch as a ref git takes: origin/HEAD's
// target, else main, else master. None is an error: the sweep never
// guesses what "merged" means.
func trunk(ctx context.Context, repo string) (string, error) {
	if ref, err := git(ctx, repo, "symbolic-ref", "--quiet", "--short", "refs/remotes/origin/HEAD"); err == nil && ref != "" && isCommit(ctx, repo, ref) {
		return ref, nil
	}
	for _, name := range []string{"main", "master"} {
		if isCommit(ctx, repo, "refs/heads/"+name) {
			return name, nil
		}
	}
	return "", errors.New("it has no default branch git can name (no origin/HEAD, main or master)")
}

func isCommit(ctx context.Context, repo, ref string) bool {
	_, err := git(ctx, repo, "rev-parse", "--verify", "--quiet", ref+"^{commit}")
	return err == nil
}

// worktree is one entry of `git worktree list --porcelain`.
type worktree struct {
	Path, Head, Branch string // Branch is the short name, "" when detached
}

// worktrees is every worktree git knows of in repo, by its real path.
func worktrees(ctx context.Context, repo string) (map[string]worktree, error) {
	out, err := git(ctx, repo, "worktree", "list", "--porcelain")
	if err != nil {
		return nil, err
	}
	known := map[string]worktree{}
	for _, block := range strings.Split(out, "\n\n") {
		w := parseWorktree(block)
		if w.Path != "" {
			known[realPath(w.Path)] = w
		}
	}
	return known, nil
}

func parseWorktree(block string) worktree {
	var w worktree
	for _, line := range strings.Split(block, "\n") {
		key, val, _ := strings.Cut(line, " ")
		switch key {
		case "worktree":
			w.Path = val
		case "HEAD":
			w.Head = val
		case "branch":
			w.Branch = strings.TrimPrefix(val, "refs/heads/")
		}
	}
	return w
}

// realPath is path with its links resolved, or as it is when it cannot be
// (a worktree whose folder is gone).
func realPath(path string) string {
	if real, err := filepath.EvalSymlinks(path); err == nil {
		return real
	}
	return filepath.Clean(path)
}

// dirty says whether a worktree has anything uncommitted, untracked files
// included.
func dirty(ctx context.Context, wt string) (bool, error) {
	out, err := git(ctx, wt, "status", "--porcelain")
	return out != "", err
}

// unmerged counts the commits of head that trunk lacks.
func unmerged(ctx context.Context, repo, trunk, head string) (int, error) {
	if head == "" {
		return 0, errors.New("git names no commit for it")
	}
	out, err := git(ctx, repo, "rev-list", "--count", trunk+".."+head)
	if err != nil {
		return 0, err
	}
	return strconv.Atoi(out)
}
