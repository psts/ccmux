package main

import (
	"context"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"ccmux.dev/ccmuxd/internal/manager"
	"ccmux.dev/ccmuxd/internal/store"
)

// A queued task's builder is a runner session (task #176), where it used to
// be a Claude typed into a tmux pane's login shell. Two things that shell
// and that plain start gave it are given back here, for builders only:
//
//   - The login shell's PATH. A session starts with the daemon's own
//     environment, whose PATH under systemd is a bare system one plus
//     ~/.local/bin (runnerEnv). A builder's hooks and the gates they run
//     (the repo's checks, the commit review) need the tools the person's
//     profile puts on PATH (go, node, uv...), the same ones the merge's own
//     checks get from their login shell (taskq.GateChecks, "bash -lc").
//   - The person's whole Claude setup. A session loads only what its group
//     and repo chose from the capability catalog (internal/loadout), and a
//     repo added after the catalog's first day starts with nothing chosen.
//     A builder built that way would run without the repo's gates and its
//     commit review and say nothing of it. So a builder has no loadout: it
//     loads what its worktree's own settings turn on (the queue copies the
//     repo's .claude/settings.local.json into it), as it did in a pane.

// sessionEnv is a session's own variables (runner.Config.SessionEnv): its
// pane's, and for a builder the login shell's PATH when that can be read.
func sessionEnv(mgr *manager.Manager, sess store.RunnerSession) map[string]string {
	env := mgr.RunnerEnv(sess.PaneID)
	if sess.Role != store.RoleBuilder {
		return env
	}
	if path := loginPath(); path != "" {
		env["PATH"] = withLocalBin(path)
	}
	return env
}

// withLocalBin is path with the user's ~/.local/bin on it, first when it
// was missing: claude, ccmuxd and ccmux-peers live there, and a profile
// that leaves it out must not hide them from a builder.
func withLocalBin(path string) string {
	home, err := os.UserHomeDir()
	if err != nil {
		return path
	}
	local := filepath.Join(home, ".local", "bin")
	for _, dir := range filepath.SplitList(path) {
		if dir == local {
			return path
		}
	}
	return local + string(os.PathListSeparator) + path
}

// sessionLoadout is the loadout of every session but a builder, which has
// none (nil is "the user's whole Claude setup", runner.Config.Loadout).
func sessionLoadout(forSession func(store.RunnerSession) ([]byte, string, error)) func(store.RunnerSession) ([]byte, string, error) {
	return func(sess store.RunnerSession) ([]byte, string, error) {
		if sess.Role == store.RoleBuilder {
			return nil, "", nil
		}
		return forSession(sess)
	}
}

var (
	loginPathOnce sync.Once
	loginPathText string
)

// loginPath is the PATH a login shell of this user has, read once: a
// profile does not change under a running daemon often enough to ask at
// every start, and asking costs a shell. "" when it cannot be read (no
// bash, a profile that hangs or prints nothing); the session then keeps the
// daemon's PATH, as every session did before, and the log says so.
func loginPath() string {
	loginPathOnce.Do(func() { loginPathText = readLoginPath(10 * time.Second) })
	return loginPathText
}

// loginPathMark fences the PATH off from whatever a profile prints.
const loginPathMark = "__ccmux_path__"

func readLoginPath(limit time.Duration) string {
	ctx, cancel := context.WithTimeout(context.Background(), limit)
	defer cancel()
	out, err := exec.CommandContext(ctx, "bash", "-lc", `printf '%s%s%s' `+loginPathMark+` "$PATH" `+loginPathMark).Output()
	path := pathBetweenMarks(string(out))
	if err != nil || path == "" {
		log.Printf("task builders: the login shell's PATH was not read (%v); builders start with the daemon's own", err)
		return ""
	}
	return path
}

// pathBetweenMarks is the text between the two marks, "" when they are not
// both there.
func pathBetweenMarks(out string) string {
	_, rest, ok := strings.Cut(out, loginPathMark)
	if !ok {
		return ""
	}
	path, _, ok := strings.Cut(rest, loginPathMark)
	if !ok {
		return ""
	}
	return strings.TrimSpace(path)
}
