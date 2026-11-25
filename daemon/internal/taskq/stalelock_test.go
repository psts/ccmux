package taskq

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// staleLock writes repo's index.lock, last written age ago.
func staleLock(t *testing.T, repo string, age time.Duration) string {
	t.Helper()
	lock := filepath.Join(repo, ".git", "index.lock")
	if err := os.WriteFile(lock, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	when := time.Now().Add(-age)
	if err := os.Chtimes(lock, when, when); err != nil {
		t.Fatal(err)
	}
	return lock
}

func exists(path string) bool { _, err := os.Lstat(path); return err == nil }

// An old empty lock with no git running in the repo is removed, and the
// path removed is the one git names.
func TestClearStaleLock_RemovesAnOldLockWithNoGit(t *testing.T) {
	repo := gitRepo(t)
	lock := staleLock(t, repo, time.Hour)
	got, err := ClearStaleLock(repo, time.Now())
	if err != nil || got != lock {
		t.Fatalf("ClearStaleLock = %q, %v; want %s removed", got, err, lock)
	}
	if exists(lock) {
		t.Fatal("the lock is still there")
	}
	if _, err := ClearStaleLock(repo, time.Now()); !errors.Is(err, ErrNoLock) {
		t.Fatalf("no lock = %v, want ErrNoLock", err)
	}
}

func TestClearStaleLock_LeavesAYoungLock(t *testing.T) {
	repo := gitRepo(t)
	lock := staleLock(t, repo, time.Minute)
	if _, err := ClearStaleLock(repo, time.Now()); err == nil || !strings.Contains(err.Error(), "may still be using it") {
		t.Fatalf("a young lock = %v, want a refusal", err)
	}
	if !exists(lock) {
		t.Fatal("a young lock was removed")
	}
}

// A process named git working in the repo keeps its lock, however old.
func TestClearStaleLock_LeavesALockWhileGitRuns(t *testing.T) {
	repo := gitRepo(t)
	lock := staleLock(t, repo, time.Hour)
	sleep, err := exec.LookPath("sleep")
	if err != nil {
		t.Skip("no sleep")
	}
	fake := filepath.Join(t.TempDir(), "git") // the kernel names the process after the file run
	if err := os.Symlink(sleep, fake); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(fake, "30")
	cmd.Dir = filepath.Join(repo, ".git") // inside the repo, in its git folder
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cmd.Process.Kill(); cmd.Wait() })
	_, err = ClearStaleLock(repo, time.Now())
	if err == nil || !strings.Contains(err.Error(), "a git command is running there") {
		t.Fatalf("a lock while git runs = %v, want a refusal", err)
	}
	if !exists(lock) {
		t.Fatal("the lock of a running git was removed")
	}
}

// A lock that is a link is never followed or removed.
func TestClearStaleLock_LeavesALinkedLock(t *testing.T) {
	repo := gitRepo(t)
	target := filepath.Join(t.TempDir(), "precious")
	if err := os.WriteFile(target, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	lock := filepath.Join(repo, ".git", "index.lock")
	if err := os.Symlink(target, lock); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-time.Hour)
	os.Chtimes(target, old, old)
	if _, err := ClearStaleLock(repo, time.Now()); err == nil || !strings.Contains(err.Error(), "not a plain file") {
		t.Fatalf("a linked lock = %v, want a refusal", err)
	}
	if !exists(lock) || !exists(target) {
		t.Fatal("a linked lock, or what it points at, was removed")
	}
}

func TestClearStaleLock_LeavesABigLock(t *testing.T) {
	repo := gitRepo(t)
	lock := staleLock(t, repo, time.Hour)
	if err := os.WriteFile(lock, make([]byte, maxLockSize+1), 0o644); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-time.Hour)
	os.Chtimes(lock, old, old)
	if _, err := ClearStaleLock(repo, time.Now()); err == nil || !strings.Contains(err.Error(), "more than a crashed git leaves") {
		t.Fatalf("a big lock = %v, want a refusal", err)
	}
}

// A host where running processes cannot be read removes nothing: "no git
// runs" is never guessed.
func TestClearStaleLock_RefusesWithoutProc(t *testing.T) {
	repo := gitRepo(t)
	lock := staleLock(t, repo, time.Hour)
	was := procDir
	procDir = filepath.Join(t.TempDir(), "no-proc")
	t.Cleanup(func() { procDir = was })
	if _, err := ClearStaleLock(repo, time.Now()); err == nil || !strings.Contains(err.Error(), "cannot list running processes") {
		t.Fatalf("no /proc = %v, want a refusal", err)
	}
	if !exists(lock) {
		t.Fatal("a lock was removed on a host that cannot tell whether git runs")
	}
}
