package main

import (
	"strings"
	"testing"

	"ccmux.dev/ccmuxd/internal/jobs"
	"ccmux.dev/ccmuxd/internal/threads"
)

func TestOnlyTheInstalledDaemonRunsJobsUnderTheDefaultName(t *testing.T) {
	if prefix, why := jobPrefix("", ""); prefix != jobs.DefaultPrefix || why != "" {
		t.Fatalf("the installed daemon uses the default template: %q %q", prefix, why)
	}
	if prefix, why := jobPrefix("", "/run/user/1000/t1"); prefix != "" || !strings.Contains(why, "-job-unit-prefix") {
		t.Fatalf("a scratch daemon that names no template runs no jobs, and says how to: %q %q", prefix, why)
	}
	if prefix, why := jobPrefix("ccmux-job-t1", "/run/user/1000/t1"); prefix != "ccmux-job-t1" || why != "" {
		t.Fatalf("a scratch daemon with its own template runs them under it: %q %q", prefix, why)
	}
}

func TestEveryJobKindTheDaemonStartsHasItsRunner(t *testing.T) {
	if _, ok := jobKinds[threads.MergeKind]; !ok {
		t.Fatal("a thread's merge has no runner: its job would be refused by job-exec")
	}
	if err := cmdJobExec(nil); err == nil || !strings.Contains(err.Error(), "usage") {
		t.Fatalf("job-exec takes one spec file: %v", err)
	}
}
