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

// fakeProc points procDir at a made-up process list for the test: pid ->
// {name, working folder}. The host's own processes (a git another session
// runs, one whose folder walls hide) then cannot change what a test sees.
func fakeProc(t *testing.T, procs map[string][2]string) {
	t.Helper()
	dir := t.TempDir()
	for pid, p := range procs {
		os.MkdirAll(filepath.Join(dir, pid), 0o755)
		if err := os.WriteFile(filepath.Join(dir, pid, "comm"), []byte(p[0]+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(p[1], filepath.Join(dir, pid, "cwd")); err != nil {
			t.Fatal(err)
		}
	}
	was := procDir
	procDir = dir
	t.Cleanup(func() { procDir = was })
}

// An old empty lock with no git running in the repo is removed, and the
// path removed is the one git names.
func TestClearStaleLock_RemovesAnOldLockWithNoGit(t *testing.T) {
	repo := gitRepo(t)
	fakeProc(t, map[string][2]string{"10": {"git", t.TempDir()}, "11": {"bash", repo}}) // a git elsewhere, a shell here
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
	fakeProc(t, nil)
	lock := staleLock(t, repo, time.Minute)
	if _, err := ClearStaleLock(repo, time.Now()); err == nil || !strings.Contains(err.Error(), "may still be using it") {
		t.Fatalf("a young lock = %v, want a refusal", err)
	}
	if !exists(lock) {
		t.Fatal("a young lock was removed")
	}
}

// A process named git working in the repo, or in its git folder, keeps its
// lock however old; so does one whose folder cannot be read.
func TestClearStaleLock_LeavesALockWhileGitRuns(t *testing.T) {
	repo := gitRepo(t)
	lock := staleLock(t, repo, time.Hour)
	for _, procs := range []map[string][2]string{
		{"20": {"git", filepath.Join(repo, ".git")}},
		{"21": {"git-remote-https", repo}},
	} {
		fakeProc(t, procs)
		if _, err := ClearStaleLock(repo, time.Now()); err == nil || !strings.Contains(err.Error(), "a git command is running there") {
			t.Fatalf("a lock while %v runs = %v, want a refusal", procs, err)
		}
	}
	dir := t.TempDir()
	os.MkdirAll(filepath.Join(dir, "30"), 0o755)
	os.WriteFile(filepath.Join(dir, "30", "comm"), []byte("git\n"), 0o644)
	os.WriteFile(filepath.Join(dir, "30", "cwd"), nil, 0o644) // not a link: reads as an error, as a folder this daemon may not see
	procDir = dir
	if _, err := ClearStaleLock(repo, time.Now()); err == nil || !strings.Contains(err.Error(), "cannot see where git process 30 works") {
		t.Fatalf("a git whose folder cannot be read = %v, want a refusal", err)
	}
	if !exists(lock) {
		t.Fatal("a lock was removed while a git may run")
	}
}

// The same with a real process the kernel names git, read from the real
// /proc. Only "refused and kept" is asserted: what else runs on the host
// may give another reason to refuse.
func TestClearStaleLock_LeavesALockWhileARealGitRuns(t *testing.T) {
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
	if _, err := os.Stat("/proc/self/cwd"); err != nil {
		t.Skip("no /proc here")
	}
	if _, err = ClearStaleLock(repo, time.Now()); err == nil {
		t.Fatal("a lock while git runs was removed")
	}
	if !exists(lock) {
		t.Fatal("the lock of a running git was removed")
	}
}

// A lock that is a link is never followed or removed.
func TestClearStaleLock_LeavesALinkedLock(t *testing.T) {
	repo := gitRepo(t)
	fakeProc(t, nil)
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
	fakeProc(t, nil)
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
