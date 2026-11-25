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
	"ccmux.dev/ccmuxd/internal/wtsweep"
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
	// sessions and jobs are the folders every session, and every job,
	// shares: a job sees nothing of the sessions' and a session nothing of
	// the jobs' (each sees its own part of its own kind's, runner.Config.plan
	// and jobs.Manager.wall). Set by wireRunners; "" for none.
	sessions, sockets, jobs string
	// caches holds the caches walled units have in place of the person's
	// own (walls.ApartCaches).
	caches string
}

func newSandbox(mgr *manager.Manager) *sandbox {
	home, _ := os.UserHomeDir()
	return &sandbox{settings: mgr.Sandbox, home: home, caches: walledCachesDir(), host: &walls.Prober{Self: selfExecutable(), Home: home}}
}

// walledCachesDir is where the walled units' own caches are kept, beside
// the sessions' state: it outlives a reboot, and a cold cache costs a
// whole build.
func walledCachesDir() string {
	return filepath.Join(filepath.Dir(runnerStateDir()), "walled-caches")
}

// secrets is what of the daemon's own a walled unit must not reach,
// because whoever holds it acts outside the walls: the folder with the
// token its API wants from this machine (apitoken.go), its config folder
// (the registry with every account's key, the secret every pane's bus
// token is made from, the node's tailnet key), and the clipboard pipe's
// token. Claude Code's own sockets are with them: each session listens
// there for messages from the others.
func secrets() []string {
	return []string{
		privateDir(), configDir(), filepath.Join(runtimeDir(), "clipboard-token"),
		fmt.Sprintf("/run/user/%d/cc-socks", os.Getuid()),
	}
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

// base is the start of every plan: caches under home that are the walled
// units' own (walls.ApartCaches), the folders a person added, what must
// refuse a write as the proof (home itself, and the folder the units' own
// files live in), what is hidden (the user's bus, the daemon's secrets),
// and Claude Code's own folder as walls has it (walls/claude.go): its
// conversations opened, what its sessions source made the unit's own, its
// config sealed.
//
// A job gets the same of Claude Code's folder as a session: a repo's
// checks may drive Claude Code (this repo's do).
func (s *sandbox) base(set walls.Settings) (*walls.Plan, error) {
	if err := walls.MakeClaudeFolders(s.home); err != nil {
		log.Printf("sandbox: a folder Claude Code needs behind walls could not be made, so a walled turn may not save its conversation: %v", err)
	}
	plan := &walls.Plan{
		Caches:  append([]string(nil), set.ExtraWrite...),
		Outside: walls.Existing([]string{s.home, systemdUserDir()}),
		Hidden:  append(userBus(), secrets()...),
	}
	if err := plan.ApartCaches(s.home, s.caches); err != nil {
		return nil, err
	}
	plan.ClaudeHome(s.home)
	return plan, nil
}

// hide adds the shared folders of the other kind of unit to what a plan
// hides.
func hide(plan *walls.Plan, folders ...string) {
	for _, dir := range folders {
		if dir != "" {
			plan.Hidden = append(plan.Hidden, dir)
		}
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
	plan, err := s.base(set)
	if err != nil {
		return nil, err
	}
	hide(plan, s.jobs)
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
	plan, err := s.base(set)
	if err != nil {
		return nil, err
	}
	hide(plan, s.sessions, s.sockets)
	// A tidy-up and the weekly sweep remove folders and branches and fetch
	// nothing. A merge runs the repo's checks, which may well fetch (a
	// module, a package).
	plan.NoNetwork = n.Kind == taskq.TidyKind || n.Kind == wtsweep.Kind
	for _, w := range work {
		if err := plan.RepoWalls(w.RepoPath); err != nil {
			return nil, fmt.Errorf("%s cannot be walled: %w", filepath.Base(w.RepoPath), err)
		}
		// The folder the worktree is in, not the worktree: the files beside
		// it are the job's too (a rebase's mark, a failed check's output,
		// the brief a tidy-up removes), and a tidy-up removes the worktree
		// itself. The sweep names no worktree: its worktrees are in the
		// repo's own folder (.claude/worktrees).
		if w.Worktree != "" {
			plan.Write = append(plan.Write, walls.Existing([]string{filepath.Dir(w.Worktree)})...)
		}
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
	case wtsweep.Input:
		work := make([]taskq.Work, len(in.Repos))
		for i, r := range in.Repos {
			work[i] = taskq.Work{RepoPath: r.Path}
		}
		return work, true
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
