package jobs

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestOneHolderOfARepoAtATime(t *testing.T) {
	dir := t.TempDir()
	first, err := LockRepos(context.Background(), dir, []string{"/p/backend", "/p/app", "/p/app/"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if LockFile(dir, "/p/app") == LockFile(dir, "/q/app") {
		t.Fatal("two repos with one folder name have a lock each")
	}
	// Another repo is free.
	other, err := LockRepos(context.Background(), dir, []string{"/p/admin"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	other.Release()
	// The same repo waits, says so once, and gives up when its time is over.
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	var waited []string
	if _, err := LockRepos(ctx, dir, []string{"/p/admin", "/p/app"}, func(repo string) { waited = append(waited, repo) }); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("a held repo is waited for until the time is over: %v", err)
	}
	if len(waited) != 1 || waited[0] != "/p/app" {
		t.Fatalf("the wait is said once, for the repo that is held: %v", waited)
	}
	// What the one that gave up had taken is free again, and so is the held
	// one once its holder lets go.
	got := make(chan error, 1)
	go func() {
		l, err := LockRepos(context.Background(), dir, []string{"/p/app", "/p/admin"}, nil)
		l.Release()
		got <- err
	}()
	time.Sleep(20 * time.Millisecond)
	first.Release()
	select {
	case err := <-got:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the lock was never handed on")
	}
}
