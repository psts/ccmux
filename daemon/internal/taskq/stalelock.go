package taskq

import (
	"errors"
	"fmt"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// A crashed git command can leave a repo's index.lock behind, and every
// merge into that repo then waits on it for good (seen 2026-11-25: an empty
// lock from three weeks before held a built task for over an hour). This
// file is the one safe way a person removes one from the task page: only
// the lock git itself names for a task's repo, only a plain small file,
// only when it is old, and only while no git process works in that repo.

// StaleLockAge is how old a lock must be before a person is asked about it
// and may have it removed: a git command that is really running holds it
// for seconds, not minutes.
const StaleLockAge = 10 * time.Minute

// maxLockSize is the largest lock file removed. A crashed git leaves an
// empty or part-written one; anything bigger is left for a person to look
// at with their own eyes.
const maxLockSize = 64 << 10

// procDir is where running processes are read from; a test points it
// elsewhere to stand for a host without /proc.
var procDir = "/proc"

// ErrNoLock is ClearStaleLock finding no lock file to remove.
var ErrNoLock = errors.New("no git lock file there")

// LockPath is the index lock git names for repo: what `git rev-parse
// --git-path index.lock` says, made absolute.
func LockPath(repo string) (string, error) {
	rel, err := runGit(repo, "rev-parse", "--git-path", "index.lock")
	if err != nil {
		return "", err
	}
	if filepath.IsAbs(rel) {
		return rel, nil
	}
	return filepath.Join(repo, rel), nil
}

// ClearStaleLock removes repo's index lock when every check holds, and
// returns its path; otherwise it removes nothing and says why. The path is
// always the one git names for repo, never one a caller passes.
func ClearStaleLock(repo string, now time.Time) (string, error) {
	lock, err := LockPath(repo)
	if err != nil {
		return "", fmt.Errorf("%s: reading where its git lock is: %w", filepath.Base(repo), err)
	}
	if err := lockRemovable(repo, lock, now); err != nil {
		return lock, err
	}
	if err := os.Remove(lock); err != nil {
		return lock, fmt.Errorf("%s: removing %s: %w", filepath.Base(repo), lock, err)
	}
	log.Printf("tasks: removed the stale git lock %s (no git command was running in %s)", lock, repo)
	return lock, nil
}

// lockRemovable checks the lock file itself, then that no git runs there.
func lockRemovable(repo, lock string, now time.Time) error {
	name := filepath.Base(repo)
	fi, err := os.Lstat(lock)
	if errors.Is(err, fs.ErrNotExist) {
		return ErrNoLock
	}
	if err != nil {
		return fmt.Errorf("%s: reading %s: %w", name, lock, err)
	}
	if !fi.Mode().IsRegular() {
		return fmt.Errorf("%s: %s is not a plain file (%s), so it is left alone", name, lock, fi.Mode().Type())
	}
	if fi.Size() > maxLockSize {
		return fmt.Errorf("%s: %s holds %d bytes, more than a crashed git leaves, so it is left alone", name, lock, fi.Size())
	}
	if age := now.Sub(fi.ModTime()); age < StaleLockAge {
		return fmt.Errorf("%s: %s is only %s old; a git command may still be using it", name, lock, age.Round(time.Second))
	}
	pid, err := GitRunningIn(repo, filepath.Dir(lock))
	if err != nil {
		return fmt.Errorf("%s: %w, so the lock is left alone", name, err)
	}
	if pid != 0 {
		return fmt.Errorf("%s: a git command is running there (process %d), so the lock is left alone", name, pid)
	}
	return nil
}

// GitRunningIn returns the id of a running process named git (or git-*)
// whose working folder is inside one of dirs, 0 for none. A host without
// /proc, or a git process whose folder cannot be read, is an error: "none"
// is never guessed.
func GitRunningIn(dirs ...string) (int, error) {
	entries, err := os.ReadDir(procDir)
	if err != nil {
		return 0, fmt.Errorf("cannot list running processes (%s: %v)", procDir, err)
	}
	roots := realDirs(dirs)
	for _, e := range entries {
		pid, err := strconv.Atoi(e.Name())
		if err != nil || !isGit(pid) {
			continue
		}
		cwd, err := os.Readlink(filepath.Join(procDir, e.Name(), "cwd"))
		if errors.Is(err, fs.ErrNotExist) {
			continue // it ended meanwhile
		}
		if err != nil {
			return 0, fmt.Errorf("cannot see where git process %d works (%v)", pid, err)
		}
		if within(cwd, roots) {
			return pid, nil
		}
	}
	return 0, nil
}

func isGit(pid int) bool {
	comm, err := os.ReadFile(filepath.Join(procDir, strconv.Itoa(pid), "comm"))
	if err != nil {
		return false
	}
	name := strings.TrimSpace(string(comm))
	return name == "git" || strings.HasPrefix(name, "git-")
}

// realDirs is each dir as /proc shows a working folder: links resolved.
func realDirs(dirs []string) []string {
	out := make([]string, 0, len(dirs))
	for _, d := range dirs {
		if real, err := filepath.EvalSymlinks(d); err == nil {
			d = real
		}
		out = append(out, filepath.Clean(d))
	}
	return out
}

func within(path string, roots []string) bool {
	for _, r := range roots {
		if path == r || strings.HasPrefix(path, r+string(filepath.Separator)) {
			return true
		}
	}
	return false
}
