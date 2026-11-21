package main

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"ccmux.dev/ccmuxd/internal/agent"
	"ccmux.dev/ccmuxd/internal/api"
	"ccmux.dev/ccmuxd/internal/harness"
	"ccmux.dev/ccmuxd/internal/jobs"
	"ccmux.dev/ccmuxd/internal/manager"
	"ccmux.dev/ccmuxd/internal/peers"
	"ccmux.dev/ccmuxd/internal/runner"
	"ccmux.dev/ccmuxd/internal/store"
	"ccmux.dev/ccmuxd/internal/taskq"
	"ccmux.dev/ccmuxd/internal/threads"
)

// runnerWiring is what wireRunners connects the session supervisor to.
type runnerWiring struct {
	st     *store.SQLite
	agents *agent.Store
	api    *api.Server
	mgr    *manager.Manager
	peers  *peers.Service // nil when the bus is off
	// prefix names the systemd unit template (-runner-unit-prefix): a scratch
	// daemon passes its own, so it never rewrites the one the live daemon's
	// sessions run under.
	prefix string
	// shim is the bus shim sessions load (-peers-shim); "" finds it.
	shim string
	// worktrees is where a thread member's worktree goes (-worktrees-dir):
	// the task queue's own folder.
	worktrees string
	// jobPrefix names the systemd unit template jobs run under
	// (-job-unit-prefix); "" is the installed daemon's, and a scratch
	// daemon that names none runs no jobs (jobs.go).
	jobPrefix string
	// refreshAfter is how long a session runs before an idle restart picks
	// up what was installed since (-session-refresh-after; runner/refresh.go).
	refreshAfter time.Duration
}

// wireRunners starts the session-runner supervisor (internal/runner): Claude
// sessions on the Agent SDK, each in its own systemd user service. Linux
// only, since it rides on systemd; anywhere else, or when systemd cannot be
// reached, the routes answer that runners are off and the daemon goes on.
//
// It returns what the job loop needs (jobs.go). The loop is started by
// main, later: a job's end is also the task queue's to act on, and the
// queue's runner is made after the bus.
func wireRunners(ctx context.Context, w runnerWiring) jobWiring {
	if runtime.GOOS != "linux" {
		return jobWiring{off: "jobs are systemd services, and this is " + runtime.GOOS}
	}
	if _, err := exec.LookPath("systemctl"); err != nil {
		log.Printf("session runners off: no systemctl on this host")
		return jobWiring{off: "no systemctl on this host"}
	}
	paths := runner.Paths{State: runnerStateDir(), Runtime: filepath.Join(runtimeDir(), "s"), Prefix: w.prefix, UnitDir: systemdUserDir()}
	units := runner.Systemctl{}
	sb := newSandbox(w.mgr)
	sb.sessions, sb.sockets = paths.State, paths.Runtime
	if err := runner.EnsureTemplate(systemdUserDir(), selfExecutable(), paths, sb.caps(), units); err != nil {
		log.Printf("session runners off: unit template: %v", err)
		return jobWiring{off: "the session runners are off, and the jobs with them"}
	}
	claude, err := harness.LookPath("claude")
	if err != nil {
		// Sessions still run, on the SDK's own copy of Claude Code, which
		// may not be the version the terminal runs.
		log.Printf("session runners: no claude on this host (%v); sessions use the SDK's bundled Claude Code", err)
	}
	cfg := runnerConfig(w, claude)
	// A thread member or a task's builder starts behind walls when the
	// Sandbox setting is on (sandbox.go).
	cfg.Walls = sb.sessionWalls
	// A thread member starts with its thread's brief, and a task's builder
	// with its part's. The threads service needs the supervisor, which needs
	// this config: so the config asks it late, once both are there.
	var team *threads.Service
	cfg.Brief = func(sess store.RunnerSession) (string, error) {
		if team == nil {
			return "", nil
		}
		return team.SessionBrief(sess)
	}
	sup := runner.New(w.st, units, paths, cfg)
	jobMgr, jobPaths, jobsOff := newJobs(w.st, w.jobPrefix, sb)
	if jobPaths != nil {
		sb.jobs = jobPaths.State
	}
	sb.serve(w.api, paths, jobPaths)
	team = newThreads(w, sup, jobMgr, jobsOff)
	w.api.SetThreads(team)
	sup.Changed = sessionChanged(w.mgr, w.peers, team)
	// Only now: what Recover does reaches the sessions through the
	// supervisor, which has to tell the panes and the bus of it (Changed).
	team.Recover()
	go expireSideHelpers(ctx, team)
	w.mgr.RunnerLiveness = sup.Liveness
	w.mgr.EndRunnerSession = func(id string) error { return sup.SetWanted(id, store.SessionDone) }
	w.api.SetRunners(sup, w.st)
	// After SetRunners: the door asks the supervisor.
	w.mgr.SleepAgentSession = w.api.SleepAgentSession
	w.api.SetRunnerDoors(api.RunnerDoors{
		Dial: func(sessionID string) api.SessionRunner { return sup.Runner(sessionID) },
		// A session whose runner is down is read from Claude Code's own store,
		// through the sidecar's export (it lands with the first session).
		History: func(ctx context.Context, sess store.RunnerSession) (json.RawMessage, error) {
			return agent.ClaudeOfflineMessages(ctx, w.agents.Root, sess.Cwd, sess.ConversationID(), "--media", sup.MediaDir(sess.ID))
		},
		Conversations: func(ctx context.Context, sess store.RunnerSession) ([]agent.OpencodeSession, error) {
			return agent.ClaudeOfflineSessions(ctx, w.agents.Root, sess.Cwd)
		},
		Past: func(ctx context.Context, sess store.RunnerSession, conv string) (json.RawMessage, error) {
			return agent.ClaudeOfflineMessages(ctx, w.agents.Root, sess.Cwd, conv, "--media", sup.MediaDir(sess.ID))
		},
	})
	go sup.Run(ctx)
	return jobWiring{mgr: jobMgr, off: jobsOff, team: team}
}

// runnerConfig is what every session's spec is built from.
func runnerConfig(w runnerWiring, claude string) runner.Config {
	return runner.Config{
		Sidecar: w.agents.EnsureClaudeServe, Claude: claude, Env: runnerEnv, Peers: peersShim(w.shim), RefreshAfter: w.refreshAfter,
		// A session's own identity: what a tmux pane's shell is given that
		// applies to Claude (its pane id and bus token, the daemon, its
		// hooks socket and its route through the LLM proxy).
		SessionEnv: func(sess store.RunnerSession) map[string]string { return sessionEnv(w.mgr, sess) },
		// What its group and repo chose from the catalog, and nothing else of
		// the user's Claude setup (loadouts.go). The day-one choices are
		// written here, before the supervisor starts a session.
		Loadout: sessionLoadout(wireLoadouts(w).ForSession),
		// A role agent's session starts on what its base says, read at
		// every start (api/agentsessions.go).
		Agent: w.api.AgentStart,
	}
}

// newThreads makes the threads service (internal/threads): threads with
// helpers, over the session supervisor, landing through the task queue's own
// merge, run as a job by jobMgr (jobs.go; nil with jobsOff saying why this
// daemon runs none). The bus asks it where a thread member's message to a repo goes, and
// has it stand that repo's helper up.
func newThreads(w runnerWiring, sup *runner.Supervisor, jobMgr *jobs.Manager, jobsOff string) *threads.Service {
	git := taskq.ExecGit{}
	team := &threads.Service{
		St: w.st, Sessions: sup, Host: w.mgr, Root: w.worktrees, Now: time.Now, MoveAfter: time.Second,
		// The checks run in the merge's job (jobs.go), with a Lander of its
		// own; this one is the daemon's git: worktrees made and removed, and
		// whether a repo's folder can take a merge.
		Lander:  taskq.Lander{Git: git, MergeGit: git, Tag: "threads"},
		Changes: taskq.ReadBranchChanges,
		// The thread page reads its rows again when a merge starts or ends.
		Changed: func(lead store.RunnerSession) { w.mgr.SessionsChanged(lead.WorkspaceID) },
		JobsOff: jobsOff,
	}
	if jobMgr != nil { // a nil manager must stay a nil Jobs, not a Jobs that holds nil
		team.Jobs = jobMgr
	}
	// A main session's side helper ends once its cache is cold, unless it
	// still owes a delegator its result (threads/sideexpire.go).
	team.Cold = sup.Cold
	team.BusName = func(paneID string) string { info, _ := w.mgr.RunnerPane(paneID); return info.Name }
	if w.peers != nil {
		wireHelpers(team, w.peers)
	}
	return team
}

// wireHelpers lets the bus route a thread member's or a main session's
// message to a repo to its own helper there, and stand that helper up: a
// main session's (Lead set) in its side thread, a thread's in its thread.
func wireHelpers(team *threads.Service, bus *peers.Service) {
	bus.ThreadRoute = func(senderPane, toName string) (peers.HelperRoute, bool) {
		r, ok := team.Route(senderPane, toName)
		return peers.HelperRoute{ThreadID: r.ThreadID, Repo: r.Repo, Name: r.Name, Lead: r.Lead, Pane: r.Pane}, ok
	}
	bus.StartHelper = func(r peers.HelperRoute) error {
		if r.Lead != "" {
			return team.StartSideHelper(r.Lead, r.Repo)
		}
		return team.StartHelper(r.ThreadID, r.Repo)
	}
	team.Owes = func(paneID string) bool { return bus.OpenTaskCountForPane(paneID) > 0 }
}

// sideExpiryEvery is how often side helpers are checked for a cold cache.
const sideExpiryEvery = time.Minute

// expireSideHelpers ends cold side helpers on a timer, for the daemon's
// life.
func expireSideHelpers(ctx context.Context, team *threads.Service) {
	t := time.NewTicker(sideExpiryEvery)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			team.ExpireSide()
		}
	}
}

// sessionChanged is what the supervisor calls when a session's row changes:
// the session's pane follows the row (made, renamed, gone with an ended
// session), and a session that is not done has its peer on the bus, which
// is what lets a message reach, and wake, one that is asleep. An ended
// session's peer leaves the bus at once, with its mailbox.
//
// A thread hears of a member that ended, off the supervisor's lock (it ends
// other sessions in turn): a lead takes its helpers with it.
func sessionChanged(mgr *manager.Manager, bus *peers.Service, team *threads.Service) func(store.RunnerSession) {
	return func(sess store.RunnerSession) {
		mgr.SyncRunnerPane(sess)
		mgr.SessionsChanged(sess.WorkspaceID)
		if team != nil {
			go teamHears(team, sess)
		}
		switch {
		case bus == nil || sess.PaneID == "":
		case sess.Wanted == store.SessionDone:
			bus.ForgetRunnerPeer(sess.PaneID)
		default:
			bus.EnsureRunnerPeer(sess.PaneID, sess.Cwd)
		}
	}
}

// teamHears tells the threads of a session's row: one that ended (a lead
// takes its helpers with it), and a main session, which its first message
// to another repo needs in the member index.
func teamHears(team *threads.Service, sess store.RunnerSession) {
	switch {
	case sess.Wanted == store.SessionDone:
		team.SessionEnded(sess)
	case sess.Kind == store.SessionLead && sess.ThreadID == 0:
		team.NoteMain(sess)
	}
}

// loadRunnerPanes rebuilds the runner sessions' panes from their rows, right
// after the manager has its workspaces and before the hooks socket opens.
func loadRunnerPanes(mgr *manager.Manager) {
	if err := mgr.LoadRunnerPanes(); err != nil {
		log.Printf("session runners: panes not rebuilt, sessions stay off the bus until the daemon restarts: %v", err)
	}
}

// runnerPaneForBus is the bus's question about a pane, answered by the
// manager from memory (the bus asks under its own lock).
func runnerPaneForBus(mgr *manager.Manager) func(paneID string) (peers.RunnerInfo, bool) {
	return func(paneID string) (peers.RunnerInfo, bool) {
		info, ok := mgr.RunnerPane(paneID)
		return peers.RunnerInfo{Name: info.Name, Repo: info.Repo, Asleep: info.Asleep}, ok
	}
}

// registerRunnerPeers puts every runner session that has not ended on the
// bus at daemon start: the bus keeps its peers in memory, and an asleep
// session has no shim to register it again.
func registerRunnerPeers(mgr *manager.Manager, bus *peers.Service) {
	for _, p := range mgr.RunnerPanes() {
		bus.EnsureRunnerPeer(p.ID, p.CWD)
	}
}

// peersShim is the bus shim a session loads as its claude-peers server: the
// one named, else the ccmux-peers beside this binary (the pair one build
// installs together), else the one on the PATH. With none, a session falls
// back to what the user's Claude config names, which joins the bus only
// from a terminal: the session runs, off the bus, and the log says so.
func peersShim(named string) string {
	if named != "" {
		return named
	}
	beside := filepath.Join(filepath.Dir(selfExecutable()), "ccmux-peers")
	if fi, err := os.Stat(beside); err == nil && !fi.IsDir() {
		return beside
	}
	if found, err := harness.LookPath("ccmux-peers"); err == nil {
		return found
	}
	log.Printf("session runners: no ccmux-peers beside %s or on the PATH; sessions will not join the peers bus", selfExecutable())
	return ""
}

// runnerStateDir holds each session's spec and state file. It must outlive
// a reboot, so it is not under the runtime folder.
func runnerStateDir() string {
	if root := scratchRoot(); root != "" {
		return filepath.Join(root, "state", "sessions")
	}
	if d := os.Getenv("XDG_STATE_HOME"); d != "" {
		return filepath.Join(d, "ccmux", "sessions")
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".local", "state", "ccmux", "sessions")
}

func systemdUserDir() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".config", "systemd", "user")
}

// runnerEnv is the environment a session starts with: the daemon's own,
// with the user's ~/.local/bin first on PATH. The daemon's service PATH is
// a bare system one (service_linux.go), and Claude's tools (claude itself,
// ccmuxd, ccmux-peers) live in ~/.local/bin.
func runnerEnv() []string {
	home, _ := os.UserHomeDir()
	local := filepath.Join(home, ".local", "bin")
	env := os.Environ()
	for i, kv := range env {
		if strings.HasPrefix(kv, "PATH=") {
			env[i] = "PATH=" + local + string(os.PathListSeparator) + strings.TrimPrefix(kv, "PATH=")
			return env
		}
	}
	return append(env, "PATH="+local+":/usr/local/bin:/usr/bin:/bin")
}

// cmdRunnerExec is the unit template's ExecStart: become the runner a
// session's spec file describes.
func cmdRunnerExec(args []string) error {
	if len(args) != 1 {
		return errors.New("usage: ccmuxd runner-exec SPEC.json")
	}
	return runner.Exec(args[0])
}
