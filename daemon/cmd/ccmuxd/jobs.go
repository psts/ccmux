package main

import (
	"context"
	"errors"
	"log"
	"os"
	"path/filepath"

	"ccmux.dev/ccmuxd/internal/jobs"
	"ccmux.dev/ccmuxd/internal/store"
	"ccmux.dev/ccmuxd/internal/taskq"
	"ccmux.dev/ccmuxd/internal/threads"
	"ccmux.dev/ccmuxd/internal/wtsweep"
)

// Job services. Long work (a thread's merge, a queued task's merge and the
// tidy-up after it) does not run in this process: each job is one run of
// the systemd user unit ccmux-job@<id>, which is this binary as `ccmuxd
// job-exec` (internal/jobs). This file is the daemon's side: the unit
// template, the manager that watches the jobs, and the verb.

// jobKinds is every kind of job this binary runs, by the name its spec
// carries. A kind is added here and given its handler in wireJobs. A
// thread's merge and a task's are the same landing, under the same lock per
// repo, so the two never work in one repo at once.
var jobKinds = map[string]jobs.Runner{
	threads.MergeKind: taskq.RunLand,
	taskq.MergeKind:   taskq.RunLand,
	taskq.TidyKind:    taskq.RunTidy,
	wtsweep.Kind:      wtsweep.Run,
}

// cmdJobExec is the unit template's ExecStart: run the job a spec file
// describes, and write its result beside it.
func cmdJobExec(args []string) error {
	if len(args) != 1 {
		return errors.New("usage: ccmuxd job-exec SPEC.json")
	}
	return jobs.Exec(args[0], jobKinds)
}

// jobsStateDir holds one folder per job (spec, journals, result). It must
// outlive a reboot, like the sessions' state beside it.
func jobsStateDir() string { return filepath.Join(filepath.Dir(runnerStateDir()), "jobs") }

// jobLocksDir holds the per-repo lock files the jobs take.
func jobLocksDir() string { return filepath.Join(filepath.Dir(runnerStateDir()), "locks") }

// jobPrefix is the name of the unit template this daemon's jobs run under,
// or "" and why it runs none. A scratch daemon (CCMUXD_ROOT) must name its
// own: the default is the installed daemon's, and a second daemon starting
// its jobs under that name would run them from the wrong folder.
func jobPrefix(flagValue, scratch string) (prefix, why string) {
	switch {
	case flagValue != "":
		return flagValue, ""
	case scratch != "":
		return "", "a scratch daemon (CCMUXD_ROOT) that named no -job-unit-prefix of its own"
	}
	return jobs.DefaultPrefix, ""
}

// newJobs makes the job manager and writes its unit template. Nil and why
// when this daemon runs no jobs; its threads then refuse to merge and say
// that reason.
func newJobs(st *store.SQLite, prefixFlag string, sb *sandbox) (*jobs.Manager, *jobs.Paths, string) {
	prefix, why := jobPrefix(prefixFlag, scratchRoot())
	if prefix == "" {
		log.Printf("jobs off: %s; threads on this daemon cannot be merged", why)
		return nil, nil, why
	}
	paths := jobs.Paths{State: jobsStateDir(), Locks: jobLocksDir(), Prefix: prefix}
	units := jobs.Systemctl{}
	if err := os.MkdirAll(paths.State, 0o700); err != nil {
		log.Printf("jobs off: %v; threads on this daemon cannot be merged", err)
		return nil, nil, err.Error()
	}
	if err := jobs.EnsureTemplate(systemdUserDir(), selfExecutable(), paths, sb.caps(), units); err != nil {
		log.Printf("jobs off: unit template: %v; threads on this daemon cannot be merged", err)
		return nil, nil, err.Error()
	}
	mgr := jobs.NewManager(st, units, paths)
	// A job is walled, or not, by what the Sandbox setting says when it is
	// made (sandbox.go).
	mgr.Walls, mgr.UnitDir = sb.jobWalls, systemdUserDir()
	return mgr, &paths, ""
}

// wireJobs gives each kind its handler and starts the manager's loop. It
// is called once everything a handler touches is wired (the supervisor's
// Changed, the bus, the task queue's runner): the loop's first look is the
// recovery after a restart, and it settles whatever ended while the daemon
// was down. queue is the task queue's handlers, by kind.
func wireJobs(ctx context.Context, w jobWiring, queue map[string]jobs.Handler) {
	if w.mgr == nil {
		return
	}
	w.mgr.Handle(threads.MergeKind, w.team.MergeHandler())
	for kind, h := range queue {
		w.mgr.Handle(kind, h)
	}
	go w.mgr.Run(ctx)
}

// jobWiring is what wireRunners leaves for the job loop, which starts
// later: the manager (nil with off saying why this daemon runs no jobs)
// and the threads service whose merges it settles.
type jobWiring struct {
	mgr  *jobs.Manager
	off  string
	team *threads.Service
}

// forQueue is the manager as the task queue takes it: a nil manager must
// stay a nil taskq.Jobs, not a Jobs that holds nil.
func (w jobWiring) forQueue() taskq.Jobs {
	if w.mgr == nil {
		return nil
	}
	return w.mgr
}
