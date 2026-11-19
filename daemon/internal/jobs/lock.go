package jobs

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"syscall"
	"time"
)

// Lock is the lock on one or more repos: while a job holds it, no other job
// rebases into, checks or moves the trunk of those repos. It is a lock file
// per repo, locked with flock, so the kernel lets go of it when the job's
// process ends, however it ends. Nothing has to tidy up after a crash.
type Lock struct{ files []*os.File }

// LockFile is the lock file of a repo under dir: named after the repo's
// folder, with a hash of its whole path so two repos called "app" do not
// share one.
func LockFile(dir, repo string) string {
	sum := sha256.Sum256([]byte(filepath.Clean(repo)))
	return filepath.Join(dir, filepath.Base(repo)+"-"+hex.EncodeToString(sum[:6])+".lock")
}

// lockPoll is how often a lock another job holds is tried again.
const lockPoll = 500 * time.Millisecond

// LockRepos takes the lock of every repo given, waiting for the ones
// another job holds, until ctx ends. They are taken in one order (by path)
// whatever order they were named in, so two jobs that want the same two
// repos never wait on each other for good. waiting is told once of each
// repo it has to wait for (nil hears nothing).
func LockRepos(ctx context.Context, dir string, repos []string, waiting func(repo string)) (*Lock, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	l := &Lock{}
	for _, repo := range uniqueSorted(repos) {
		f, err := lockOne(ctx, LockFile(dir, repo), func() {
			if waiting != nil {
				waiting(repo)
			}
		})
		if err != nil {
			l.Release()
			return nil, fmt.Errorf("the lock on %s: %w", filepath.Base(repo), err)
		}
		l.files = append(l.files, f)
	}
	return l, nil
}

func uniqueSorted(repos []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, r := range repos {
		r = filepath.Clean(r)
		if !seen[r] {
			seen[r] = true
			out = append(out, r)
		}
	}
	sort.Strings(out)
	return out
}

// lockOne locks one file, asking again every lockPoll while another process
// holds it: a plain blocking flock could not be given up at ctx's end.
func lockOne(ctx context.Context, path string, waiting func()) (*os.File, error) {
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, err
	}
	told := false
	for {
		err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			return f, nil
		}
		if !errors.Is(err, syscall.EWOULDBLOCK) && !errors.Is(err, syscall.EINTR) {
			f.Close()
			return nil, err
		}
		if !told {
			told = true
			waiting()
		}
		select {
		case <-ctx.Done():
			f.Close()
			return nil, ctx.Err()
		case <-time.After(lockPoll):
		}
	}
}

// Release lets go of every lock. A job that simply ends needs no call: the
// kernel does it.
func (l *Lock) Release() {
	if l == nil {
		return
	}
	for _, f := range l.files {
		f.Close()
	}
	l.files = nil
}
