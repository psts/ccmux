package taskq

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// Git is the runner's view of git, faked in tests.
type Git interface {
	// Trunk names the branch a task builds from and merges back to.
	Trunk(repo string) (string, error)
	// AddWorktree makes path a worktree of repo on branch, creating the
	// branch from base when it does not exist yet. A path that is already
	// a worktree is kept as it is: a retried task carries on from its work.
	AddWorktree(repo, path, branch, base string) error
}

// ExecGit runs the git binary.
type ExecGit struct{}

const gitTimeout = time.Minute

func runGit(dir string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), gitTimeout)
	defer cancel()
	out, err := exec.CommandContext(ctx, "git", append([]string{"-C", dir}, args...)...).CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("git %s: %v: %s", strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return strings.TrimSpace(string(out)), nil
}

func branchExists(repo, branch string) bool {
	_, err := runGit(repo, "rev-parse", "--verify", "--quiet", "refs/heads/"+branch)
	return err == nil
}

// Trunk is dev where the repo has one (work happens on dev and only /ship
// goes to main), else main, else master, else the branch checked out.
func (ExecGit) Trunk(repo string) (string, error) {
	for _, b := range []string{"dev", "main", "master"} {
		if branchExists(repo, b) {
			return b, nil
		}
	}
	return runGit(repo, "symbolic-ref", "--short", "HEAD")
}

func (ExecGit) AddWorktree(repo, path, branch, base string) error {
	if _, err := os.Stat(filepath.Join(path, ".git")); err == nil {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	// A worktree folder deleted by hand is still registered with git, and
	// git refuses to add one at that path again until it is pruned.
	if _, err := runGit(repo, "worktree", "prune"); err != nil {
		return err
	}
	if branchExists(repo, branch) {
		_, err := runGit(repo, "worktree", "add", path, branch)
		return err
	}
	_, err := runGit(repo, "worktree", "add", "-b", branch, path, base)
	return err
}

// copyLocalSettings gives the worktree the repo's untracked Claude settings,
// which is where a repo turns its plugins and quality gates on. Without it
// a worker would build with none of the gates its human works under.
func copyLocalSettings(repo, worktree string) error {
	src := filepath.Join(repo, ".claude", "settings.local.json")
	dst := filepath.Join(worktree, ".claude", "settings.local.json")
	data, err := os.ReadFile(src)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if _, err := os.Stat(dst); err == nil {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	return os.WriteFile(dst, data, 0o644)
}
