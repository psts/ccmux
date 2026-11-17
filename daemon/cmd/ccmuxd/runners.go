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
}

// wireRunners starts the session-runner supervisor (internal/runner): Claude
// sessions on the Agent SDK, each in its own systemd user service. Linux
// only, since it rides on systemd; anywhere else, or when systemd cannot be
// reached, the routes answer that runners are off and the daemon goes on.
func wireRunners(ctx context.Context, w runnerWiring) {
	if runtime.GOOS != "linux" {
		return
	}
	if _, err := exec.LookPath("systemctl"); err != nil {
		log.Printf("session runners off: no systemctl on this host")
		return
	}
	paths := runner.Paths{State: runnerStateDir(), Runtime: filepath.Join(runtimeDir(), "s"), Prefix: w.prefix}
	units := runner.Systemctl{}
	if err := runner.EnsureTemplate(systemdUserDir(), selfExecutable(), paths, units); err != nil {
		log.Printf("session runners off: unit template: %v", err)
		return
	}
	claude, err := harness.LookPath("claude")
	if err != nil {
		// Sessions still run, on the SDK's own copy of Claude Code, which
		// may not be the version the terminal runs.
		log.Printf("session runners: no claude on this host (%v); sessions use the SDK's bundled Claude Code", err)
	}
	cfg := runner.Config{
		Sidecar: w.agents.EnsureClaudeServe, Claude: claude, Env: runnerEnv, Peers: peersShim(w.shim),
		// A session's own identity: what a tmux pane's shell is given that
		// applies to Claude (its pane id and bus token, the daemon, its
		// hooks socket and its route through the LLM proxy).
		SessionEnv: func(sess store.RunnerSession) map[string]string { return w.mgr.RunnerEnv(sess.PaneID) },
	}
	// A thread member starts with its thread's brief. The threads service
	// needs the supervisor, which needs this config: so the config asks it
	// late, once both are there.
	var team *threads.Service
	cfg.Brief = func(sess store.RunnerSession) string {
		if team == nil {
			return ""
		}
		return team.Brief(sess)
	}
	sup := runner.New(w.st, units, paths, cfg)
	team = newThreads(w, sup)
	w.api.SetThreads(team)
	sup.Changed = sessionChanged(w.mgr, w.peers, team)
	w.mgr.RunnerLiveness = sup.Liveness
	w.mgr.EndRunnerSession = func(id string) {
		if err := sup.SetWanted(id, store.SessionDone); err != nil {
			log.Printf("session runners: session %s of a deleted workspace was not ended: %v", id, err)
		}
	}
	w.api.SetRunners(sup, w.st)
	w.api.SetRunnerDoors(api.RunnerDoors{
		Dial: func(sessionID string) api.SessionRunner { return sup.Runner(sessionID) },
		// A session whose runner is down is read from Claude Code's own store,
		// through the sidecar's export (it lands with the first session).
		History: func(ctx context.Context, sess store.RunnerSession) (json.RawMessage, error) {
			return agent.ClaudeOfflineMessages(ctx, w.agents.Root, sess.Cwd, sess.ID, "--media", sup.MediaDir(sess.ID))
		},
	})
	go sup.Run(ctx)
}

// newThreads makes the threads service (internal/threads): threads with
// helpers, over the session supervisor, landing through the task queue's own
// merge. The bus asks it where a thread member's message to a repo goes, and
// has it stand that repo's helper up.
func newThreads(w runnerWiring, sup *runner.Supervisor) *threads.Service {
	git := taskq.ExecGit{}
	team := &threads.Service{
		St: w.st, Sessions: sup, Host: w.mgr, Root: w.worktrees, Now: time.Now, MoveAfter: time.Second,
		Lander:  taskq.Lander{Git: git, MergeGit: git, Checks: taskq.GateChecks{PerLine: 20 * time.Minute}, Tag: "threads"},
		Changes: taskq.ReadBranchChanges,
	}
	if w.peers != nil {
		w.peers.ThreadRoute = func(senderPane, toName string) (peers.HelperRoute, bool) {
			r, ok := team.Route(senderPane, toName)
			return peers.HelperRoute{ThreadID: r.ThreadID, Repo: r.Repo, Name: r.Name}, ok
		}
		w.peers.StartHelper = team.StartHelper
	}
	team.Recover()
	return team
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
		if team != nil && sess.Wanted == store.SessionDone {
			go team.SessionEnded(sess)
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
