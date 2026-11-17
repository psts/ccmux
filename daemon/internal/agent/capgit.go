package agent

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Git for the catalog's sources: ccmux keeps its own checkout of each
// (never ~/.claude/plugins, which the user's terminal Claude owns). The
// host's git credential setup applies, so a private GitHub source clones
// through the gh helper when the host has one configured.

// gitTimeout bounds one git command, so a hung remote cannot hold gitMu.
const gitTimeout = 3 * time.Minute

// gitMu serializes the git work on every source checkout and pin folder.
// It is taken before capMu, never inside it: a clone takes seconds and
// capMu must not wait on the network.
var gitMu sync.Mutex

// runGit runs git in dir with no terminal prompt, so a missing credential
// fails at once instead of waiting on a prompt nobody sees.
func runGit(ctx context.Context, dir string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, gitTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0", "GCM_INTERACTIVE=never")
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("git %s: %w: %s", args[0], err, strings.TrimSpace(string(out)))
	}
	return strings.TrimSpace(string(out)), nil
}

// syncCheckout clones url into dir, or moves the checkout already there to
// its remote's branch, and returns the commit it is at. It resets rather
// than merges: the checkout is ccmux's own and nothing edits it, so it
// mirrors the remote, and a force-pushed source updates like any other
// instead of refusing every update after. Pins are worktrees of their own,
// which a reset never moves, and their HEADs keep their commits alive.
func syncCheckout(ctx context.Context, url, dir string) (string, error) {
	if _, err := os.Stat(filepath.Join(dir, ".git")); errors.Is(err, os.ErrNotExist) {
		if err := cloneInto(ctx, url, dir); err != nil {
			return "", err
		}
	} else {
		if _, err := runGit(ctx, dir, "fetch", "--quiet", "origin"); err != nil {
			return "", err
		}
		if _, err := runGit(ctx, dir, "reset", "--hard", "--quiet", "@{upstream}"); err != nil {
			return "", err
		}
	}
	return runGit(ctx, dir, "rev-parse", "HEAD")
}

// cloneInto clones url beside dir and moves it into place only once the
// clone is whole, so a clone cut off (a closed request, the timeout, a
// daemon restart) leaves no half checkout that every later update trips on.
func cloneInto(ctx context.Context, url, dir string) error {
	parent := filepath.Dir(dir)
	if err := os.MkdirAll(parent, 0o755); err != nil {
		return err
	}
	tmp, err := os.MkdirTemp(parent, "."+filepath.Base(dir)+".clone-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmp)
	if _, err := runGit(ctx, tmp, "clone", "--quiet", "--", url, "repo"); err != nil {
		return err
	}
	if err := os.RemoveAll(dir); err != nil {
		return err
	}
	return os.Rename(filepath.Join(tmp, "repo"), dir)
}

// folderCommit is the short commit that last changed rel in the checkout,
// so a plugin's version moves only when its own folder does.
func folderCommit(ctx context.Context, dir, rel string) string {
	out, err := runGit(ctx, dir, "log", "-1", "--format=%h", "--", rel)
	if err != nil {
		return ""
	}
	return out
}

// pinFolder is a detached worktree of the checkout at commit, made once:
// a pinned entry points inside it, so later fetches never move it. A
// worktree rather than a copied folder: it shares the checkout's objects,
// costs one checkout of disk, and git itself says what is in it.
func pinFolder(ctx context.Context, checkout, pins, commit string) (string, error) {
	dir := filepath.Join(pins, commit)
	if _, err := os.Stat(dir); err == nil {
		return dir, nil
	}
	if err := os.MkdirAll(pins, 0o755); err != nil {
		return "", err
	}
	_, err := runGit(ctx, checkout, "worktree", "add", "--detach", "--quiet", dir, commit)
	return dir, err
}
