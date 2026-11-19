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
)

// Job services. Long work (a thread's merge today, the task queue's checks
// and landing next) does not run in this process: each job is one run of
// the systemd user unit ccmux-job@<id>, which is this binary as `ccmuxd
// job-exec` (internal/jobs). This file is the daemon's side: the unit
// template, the manager that watches the jobs, and the verb.

// jobKinds is every kind of job this binary runs, by the name its spec
// carries. A kind is added here and given its handler in wireJobs.
var jobKinds = map[string]jobs.Runner{
	threads.MergeKind: taskq.RunLand,
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
func newJobs(st *store.SQLite, prefixFlag string) (*jobs.Manager, string) {
	prefix, why := jobPrefix(prefixFlag, scratchRoot())
	if prefix == "" {
		log.Printf("jobs off: %s; threads on this daemon cannot be merged", why)
		return nil, why
	}
	paths := jobs.Paths{State: jobsStateDir(), Locks: jobLocksDir(), Prefix: prefix}
	units := jobs.Systemctl{}
	if err := os.MkdirAll(paths.State, 0o700); err != nil {
		log.Printf("jobs off: %v; threads on this daemon cannot be merged", err)
		return nil, err.Error()
	}
	if err := jobs.EnsureTemplate(systemdUserDir(), selfExecutable(), paths, jobs.DefaultLimits(), units); err != nil {
		log.Printf("jobs off: unit template: %v; threads on this daemon cannot be merged", err)
		return nil, err.Error()
	}
	return jobs.NewManager(st, units, paths), ""
}

// wireJobs gives each kind its handler and starts the manager's loop. It
// is called once everything a handler touches is wired (the supervisor's
// Changed above all): the loop's first look is the recovery after a
// restart, and it settles whatever ended while the daemon was down.
func wireJobs(ctx context.Context, m *jobs.Manager, team *threads.Service) {
	if m == nil {
		return
	}
	m.Handle(threads.MergeKind, team.MergeHandler())
	go m.Run(ctx)
}
