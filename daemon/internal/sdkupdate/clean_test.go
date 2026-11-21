package sdkupdate

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"syscall"
	"testing"
	"time"
)

// A trial cut off by a daemon restart leaves its runner up and its folder:
// the next start stops that runner and removes the folder. A pid reused
// by another process is not killed, and another daemon's trials are left.
func TestCleanTrialsStopsWhatACutOffTrialLeft(t *testing.T) {
	tag := TrialTag(t.TempDir())
	mine, err := os.MkdirTemp("", tag)
	if err != nil {
		t.Fatal(err)
	}
	reused, _ := os.MkdirTemp("", tag)
	other, _ := os.MkdirTemp("", TrialTag(t.TempDir()))
	t.Cleanup(func() { os.RemoveAll(mine); os.RemoveAll(reused); os.RemoveAll(other) })
	// A stand-in runner whose command line names its trial folder, in its
	// own process group as a real one is.
	runner := exec.Command("sh", "-c", "sleep 60", filepath.Join(mine, "work", "x"))
	runner.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := runner.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- runner.Wait() }()
	write := func(root string, pid int) {
		if err := os.WriteFile(filepath.Join(root, "pid"), []byte(strconv.Itoa(pid)), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write(mine, runner.Process.Pid)
	write(reused, os.Getpid()) // this test's own pid: not a runner of that folder
	write(other, runner.Process.Pid)
	CleanTrials(tag)
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		runner.Process.Kill()
		t.Fatal("the left runner was not stopped")
	}
	for root, want := range map[string]bool{mine: false, reused: false, other: true} {
		if _, err := os.Stat(root); (err == nil) != want {
			t.Errorf("%s kept = %v, want %v", root, err == nil, want)
		}
	}
}

// The cleanup runs once, at start, before the first try.
func TestRunCleansUpFirst(t *testing.T) {
	var order []string
	st := &fakeStore{pinned: "0.3.311"}
	u, _ := updater(st, "", nil)
	u.Latest = func() (string, error) { order = append(order, "try"); return "", errors.New("offline") }
	u.Cleanup = func() { order = append(order, "cleanup") }
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	u.Run(ctx)
	if len(order) != 2 || order[0] != "cleanup" || order[1] != "try" {
		t.Fatalf("order %v", order)
	}
}
