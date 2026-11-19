package main

import (
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"

	"ccmux.dev/ccmuxd/internal/api"
	"ccmux.dev/ccmuxd/internal/jobs"
	"ccmux.dev/ccmuxd/internal/manager"
	"ccmux.dev/ccmuxd/internal/runner"
	"ccmux.dev/ccmuxd/internal/store"
	"ccmux.dev/ccmuxd/internal/taskq"
	"ccmux.dev/ccmuxd/internal/walls"
)

// The sandbox (internal/walls) as this daemon applies it: which units are
// walled and into what, from the Sandbox tab's settings, read at every
// start of a unit. The caps are in the two unit templates; the walls are a
// drop-in file per unit, and each walled unit proves them itself before it
// does any work.
//
// What is walled is task and thread work: a thread's members and a task's
// builders (the sessions with a role), and the jobs that merge and tidy
// their work. A repo's own session, a person's plain session and a role
// agent run open, under the caps only: they work in the person's own
// folders, by design.

// sandbox is the daemon's side of the walls.
type sandbox struct {
	settings func() (walls.Settings, error)
	host     *walls.Prober
	home     string
	// templates writes the two unit templates with the caps given; set by
	// wireRunners once it knows the templates' names.
	templates func(walls.Caps) error
}

func newSandbox(mgr *manager.Manager) *sandbox {
	home, _ := os.UserHomeDir()
	return &sandbox{settings: mgr.Sandbox, home: home, host: &walls.Prober{Self: selfExecutable(), Home: home}}
}

// serve gives the Settings page its Sandbox tab, now that both templates'
// names are known, and asks the host once what it allows, off the start's
// path: the page then has the answer, and so has the first walled start.
func (s *sandbox) serve(api *api.Server, runnerPaths runner.Paths, jobPaths *jobs.Paths) {
	s.templates = writeTemplates(runnerPaths, jobPaths)
	api.SetSandbox(s)
	go s.Host(true)
}

// caps are the caps the unit templates are written with at this daemon's
// start. Settings that cannot be read are the defaults for that: a
// template has to say something, and the defaults only stop a runaway.
func (s *sandbox) caps() walls.Caps {
	set, err := s.settings()
	if err != nil {
		log.Printf("sandbox: settings unreadable, the units' caps are the defaults: %v", err)
		set = walls.Defaults()
	}
	return set.Caps()
}

// Host is what this host allows: asked now when fresh, else the last
// answer.
func (s *sandbox) Host(fresh bool) walls.Host {
	if fresh {
		return s.host.Probe()
	}
	return s.host.Last()
}

// Apply makes the units' caps what the settings say now. systemd applies
// them to the running units too.
func (s *sandbox) Apply(set walls.Settings) error {
	if s.templates == nil {
		return nil
	}
	return s.templates(set.Caps())
}

// on reports whether a unit that does task or thread work is walled now.
// An error is a unit that must not start: the settings could not be read,
// or the sandbox is on and this host walls nothing.
func (s *sandbox) on() (walls.Settings, bool, error) {
	set, err := s.settings()
	if err != nil {
		return set, false, fmt.Errorf("the sandbox settings could not be read, so nothing is started on a guess: %w", err)
	}
	if !set.On {
		return set, false, nil
	}
	return set, true, s.host.Ready()
}

// base is the start of every plan: the caches under home, Claude Code's
// own folder, the folders a person added, what must refuse a write as the
// proof (home itself, and the folder the units' own files live in), and
// the sockets that are hidden.
//
// Claude Code's folder is open to a job too, not only to a session: a
// repo's checks may drive Claude Code. This repo's do, and 40 of its tests
// failed behind walls without it (measured 2026-11-19).
func (s *sandbox) base(set walls.Settings) *walls.Plan {
	return &walls.Plan{
		Caches:  append(append(walls.Caches(s.home), walls.ClaudeHome(s.home)...), set.ExtraWrite...),
		Outside: walls.Existing([]string{s.home, systemdUserDir()}),
		Hidden:  userBus(),
	}
}

// userBus is the two sockets through which a process of this user asks
// its systemd to run something: the user's bus, and systemd's own socket.
// A walled unit must reach neither, or `systemd-run --user` is its way out.
func userBus() []string {
	dir := fmt.Sprintf("/run/user/%d", os.Getuid())
	return []string{filepath.Join(dir, "bus"), filepath.Join(dir, "systemd", "private")}
}

// sessionWalls is runner.Config.Walls: a thread member or a task's builder
// is walled into its worktree, plus what Claude Code writes under home. A
// member that only reads yet (it is still in its repo's own folder) gets
// no worktree: its repo stays read-only, as its fence has it.
func (s *sandbox) sessionWalls(sess store.RunnerSession) (*walls.Plan, error) {
	if sess.Role == "" {
		return nil, nil
	}
	set, on, err := s.on()
	if err != nil || !on {
		return nil, err
	}
	plan := s.base(set)
	// Claude Code's own sandbox around its shell commands too, when the
	// person asked for it. A host that lacks what it needs starts nothing:
	// asked for and silently left out is the one thing walls never are.
	if plan.ShellBox = set.ShellBox; set.ShellBox && !s.host.Last().ClaudeSandbox() {
		return nil, errors.New("the sandbox for shell commands is on, and this host lacks what it needs (bubblewrap that works, and socat). " + walls.Fix)
	}
	if sess.Worktree == "" || sess.Cwd != sess.Worktree {
		return plan, nil
	}
	if err := plan.WorktreeWalls(sess.Worktree); err != nil {
		return nil, fmt.Errorf("its worktree cannot be walled: %w", err)
	}
	return plan, nil
}

// jobWalls is jobs.Manager.Walls: a merge or a tidy-up is walled into the
// repos it lands in and the folders its worktrees are in. With the sandbox
// on, a job whose work the walls do not know is not started at all: it
// would run open while the page says walled. A new kind of job gets its
// case in workOf.
func (s *sandbox) jobWalls(n jobs.New) (*walls.Plan, error) {
	set, on, err := s.on()
	if err != nil || !on {
		return nil, err
	}
	work, known := workOf(n)
	if !known {
		return nil, fmt.Errorf("the sandbox is on, and a job of kind %s is not one its walls know (sandbox.go, workOf): it is not started open", n.Kind)
	}
	plan := s.base(set)
	// A tidy-up removes folders and branches and fetches nothing. A merge
	// runs the repo's checks, which may well fetch (a module, a package).
	plan.NoNetwork = n.Kind == taskq.TidyKind
	for _, w := range work {
		if err := plan.RepoWalls(w.RepoPath); err != nil {
			return nil, fmt.Errorf("%s cannot be walled: %w", filepath.Base(w.RepoPath), err)
		}
		// The folder the worktree is in, not the worktree: the files beside
		// it are the job's too (a rebase's mark, a failed check's output,
		// the brief a tidy-up removes), and a tidy-up removes the worktree
		// itself.
		plan.Write = append(plan.Write, walls.Existing([]string{filepath.Dir(w.Worktree)})...)
	}
	return plan, nil
}

// workOf is the repos and worktrees a job works in, read off what it is
// started from; false for a job this does not know.
func workOf(n jobs.New) ([]taskq.Work, bool) {
	switch in := n.Input.(type) {
	case taskq.LandInput:
		return in.Work, true
	case taskq.TidyInput:
		return in.Work, true
	}
	return nil, false
}

// writeTemplates is sandbox.templates for a daemon whose sessions run under
// runnerPaths and whose jobs under jobPaths (nil for a daemon that runs no
// jobs).
func writeTemplates(runnerPaths runner.Paths, jobPaths *jobs.Paths) func(walls.Caps) error {
	return func(caps walls.Caps) error {
		if err := runner.EnsureTemplate(systemdUserDir(), selfExecutable(), runnerPaths, caps, runner.Systemctl{}); err != nil {
			return fmt.Errorf("the sessions' unit template: %w", err)
		}
		if jobPaths == nil {
			return nil
		}
		if err := jobs.EnsureTemplate(systemdUserDir(), selfExecutable(), *jobPaths, caps, jobs.Systemctl{}); err != nil {
			return fmt.Errorf("the jobs' unit template: %w", err)
		}
		return nil
	}
}

// cmdWallsProbe is the trial unit's ExecStart (walls.Prober): check that
// this process is behind walls, and exit with what it found.
func cmdWallsProbe(args []string) error {
	os.Exit(walls.RunProbe(args))
	return nil
}
