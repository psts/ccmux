package taskq

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// MergeGit is the git a merge needs, faked in tests.
type MergeGit interface {
	// TrunkBlocked says why repo's trunk cannot be moved forward now ("" when
	// it can): it is checked out in the repo's own folder with uncommitted
	// changes there, or checked out in another worktree.
	TrunkBlocked(repo, trunk string) (string, error)
	// Uncommitted lists what the worktree holds that is not committed ("" for
	// nothing): the merge takes commits only, so a check that passed on such
	// a file proves nothing about what lands.
	Uncommitted(worktree string) (string, error)
	// Rebase replays the worktree's branch onto trunk, undoing a clash, and
	// returns the commit it ended on: the one checked and merged.
	Rebase(worktree, trunk string) (string, error)
	// FastForward moves repo's trunk to rev, never rewriting it.
	FastForward(repo, rev, trunk string) error
	// ChangedUnder lists the files under dir that rev changes against trunk.
	ChangedUnder(repo, trunk, rev, dir string) ([]string, error)
	// RemoveWorktree removes a merged part's worktree and its branch.
	RemoveWorktree(repo, path, branch, trunk string) error
}

// localSettings is the file the runner copies into every worktree
// (copyLocalSettings); it is never the worker's work.
const localSettings = ".claude/settings.local.json"

// checkedOut names the branch the repo's own folder has checked out, ""
// for a detached HEAD. Any other failure is an error, never a guess: ""
// would read as "the trunk is not checked out here" and pick the path that
// moves the ref under the folder's feet.
func checkedOut(repo string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), gitTimeout)
	defer cancel()
	out, err := exec.CommandContext(ctx, "git", "-C", repo, "symbolic-ref", "--short", "-q", "HEAD").Output()
	var exit *exec.ExitError
	if errors.As(err, &exit) && exit.ExitCode() == 1 {
		return "", nil // detached: no branch checked out
	}
	if err != nil {
		return "", fmt.Errorf("reading the branch checked out in %s: %w", repo, err)
	}
	return strings.TrimSpace(string(out)), nil
}

func (ExecGit) TrunkBlocked(repo, trunk string) (string, error) {
	if elsewhere, err := trunkElsewhere(repo, trunk); err != nil || elsewhere != "" {
		return elsewhere, err
	}
	cur, err := checkedOut(repo)
	if err != nil {
		return "", err
	}
	if cur != trunk {
		return "", nil // the ref moves without touching anyone's files
	}
	out, err := runGit(repo, "status", "--porcelain", "--untracked-files=no")
	if err != nil {
		return "", err
	}
	if out != "" {
		return fmt.Sprintf("%s has uncommitted changes on %s", filepath.Base(repo), trunk), nil
	}
	return "", nil
}

// trunkElsewhere names a worktree other than the repo's own folder that has
// trunk checked out: moving the ref under it would leave its files behind.
func trunkElsewhere(repo, trunk string) (string, error) {
	out, err := runGit(repo, "worktree", "list", "--porcelain")
	if err != nil {
		return "", err
	}
	path := ""
	for i, line := range strings.Split(out, "\n") {
		if p, ok := strings.CutPrefix(line, "worktree "); ok {
			path = p
			if i == 0 {
				path = "" // the first entry is the repo's own folder
			}
		}
		if line == "branch refs/heads/"+trunk && path != "" {
			return fmt.Sprintf("%s is checked out in %s", trunk, path), nil
		}
	}
	return "", nil
}

func (ExecGit) Uncommitted(worktree string) (string, error) {
	out, err := runGit(worktree, "status", "--porcelain", "--untracked-files=all")
	if err != nil {
		return "", err
	}
	var left []string
	for _, line := range strings.Split(out, "\n") {
		if line != "" && !strings.HasSuffix(line, " "+localSettings) {
			left = append(left, strings.TrimSpace(line))
		}
	}
	return strings.Join(left, ", "), nil
}

func (ExecGit) Rebase(worktree, trunk string) (string, error) {
	if _, err := runGit(worktree, "rebase", trunk); err != nil {
		_, _ = runGit(worktree, "rebase", "--abort")
		return "", fmt.Errorf("its branch clashes with %s: %w", trunk, err)
	}
	return runGit(worktree, "rev-parse", "HEAD")
}

// FastForward merges in the repo's own folder when the trunk is checked out
// there (the folder is clean: TrunkBlocked said so); otherwise it moves the
// trunk ref alone, after checking it is a fast-forward, and only from the
// value it checked.
func (ExecGit) FastForward(repo, rev, trunk string) error {
	cur, err := checkedOut(repo)
	if err != nil {
		return err
	}
	if cur == trunk {
		_, err := runGit(repo, "merge", "--ff-only", rev)
		return err
	}
	old, err := runGit(repo, "rev-parse", "refs/heads/"+trunk)
	if err != nil {
		return err
	}
	if _, err := runGit(repo, "merge-base", "--is-ancestor", old, rev); err != nil {
		return fmt.Errorf("%s moved since the rebase; try the merge again", trunk)
	}
	_, err = runGit(repo, "update-ref", "refs/heads/"+trunk, rev, old)
	return err
}

// RemoveWorktree removes the worktree and then the branch, forcing
// neither. The local settings the runner copied in go first, when the repo
// does not track them; then git's own remove refuses a worktree with
// anything uncommitted (work written after the merge checked, say). The
// branch goes only when the trunk holds every commit on it: git's own safe
// delete judges against the folder's current branch instead, which is not
// the trunk when the folder sits on another branch.
func (ExecGit) RemoveWorktree(repo, path, branch, trunk string) error {
	if _, err := runGit(path, "ls-files", "--error-unmatch", localSettings); err != nil {
		if rmErr := os.Remove(filepath.Join(path, localSettings)); rmErr != nil && !os.IsNotExist(rmErr) {
			return rmErr
		}
		_ = os.Remove(filepath.Join(path, ".claude")) // only when that left it empty
	}
	if _, err := runGit(repo, "worktree", "remove", path); err != nil {
		return fmt.Errorf("kept, it holds uncommitted files: %w", err)
	}
	if _, err := runGit(repo, "merge-base", "--is-ancestor", branch, trunk); err != nil {
		return fmt.Errorf("worktree removed, branch %s kept: it has commits %s does not", branch, trunk)
	}
	if _, err := runGit(repo, "branch", "-D", branch); err != nil {
		return fmt.Errorf("worktree removed, branch %s not deleted: %w", branch, err)
	}
	return nil
}

// ChangedUnder lists the files under dir that differ between trunk and rev:
// a rename as both its paths, names as they are (unquoted).
func (ExecGit) ChangedUnder(repo, trunk, rev, dir string) ([]string, error) {
	out, err := runGit(repo, "diff", "--name-only", "--no-renames", "-z", trunk, rev, "--", dir)
	if err != nil {
		return nil, err
	}
	var paths []string
	for _, p := range strings.Split(out, "\x00") {
		if p != "" {
			paths = append(paths, p)
		}
	}
	return paths, nil
}
