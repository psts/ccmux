package main

import (
	"strings"
	"testing"

	"ccmux.dev/ccmuxd/internal/jobs"
	"ccmux.dev/ccmuxd/internal/taskq"
	"ccmux.dev/ccmuxd/internal/threads"
	"ccmux.dev/ccmuxd/internal/wtsweep"
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
	for _, kind := range []string{threads.MergeKind, taskq.MergeKind, taskq.TidyKind, wtsweep.Kind} {
		if _, ok := jobKinds[kind]; !ok {
			t.Fatalf("a %s job has no runner: it would be refused by job-exec", kind)
		}
	}
	if err := cmdJobExec(nil); err == nil || !strings.Contains(err.Error(), "usage") {
		t.Fatalf("job-exec takes one spec file: %v", err)
	}
}

// A daemon with no job manager hands the task queue a plain nil, so the
// queue says "this daemon runs no jobs" instead of calling into nothing.
func TestTheQueueGetsNoJobsFromADaemonThatRunsNone(t *testing.T) {
	if got := (jobWiring{off: "no systemctl on this host"}).forQueue(); got != nil {
		t.Fatalf("a nil manager reached the queue as a Jobs that is not nil: %#v", got)
	}
	if got := (jobWiring{mgr: &jobs.Manager{}}).forQueue(); got == nil {
		t.Fatal("a daemon that runs jobs hands the queue none")
	}
	wireJobs(t.Context(), jobWiring{off: "no systemctl on this host"}, nil) // nothing to start, and no panic
}
