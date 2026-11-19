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

// A role agent's session (task #177) is given the same two, for the same
// reasons: it ran in a pane's login shell too, where its MCP servers (npx,
// uvx) and its shell commands found the person's tools, and its base, not
// the catalog, says what it loads.

// ownSetup reports a session that brings its own setup instead of a
// loadout, and needs the login shell's PATH: a task's builder and a role
// agent.
func ownSetup(sess store.RunnerSession) bool {
	return sess.Role == store.RoleBuilder || sess.Kind == store.SessionAgent
}

// sessionEnv is a session's own variables (runner.Config.SessionEnv): its
// pane's, and for a builder or an agent the login shell's PATH when that
// can be read.
func sessionEnv(mgr *manager.Manager, sess store.RunnerSession) map[string]string {
	env := mgr.RunnerEnv(sess.PaneID)
	if !ownSetup(sess) {
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

// sessionLoadout is the loadout of every session but a builder and a role
// agent, which have none (nil is "the user's whole Claude setup",
// runner.Config.Loadout; an agent's own flags then narrow it).
func sessionLoadout(forSession func(store.RunnerSession) ([]byte, string, error)) func(store.RunnerSession) ([]byte, string, error) {
	return func(sess store.RunnerSession) ([]byte, string, error) {
		if ownSetup(sess) {
			return nil, "", nil
		}
		return forSession(sess)
	}
}

// pathCache keeps the login shell's PATH once it has been read. Only an
// answer is kept for good: a read that failed (a boot or a deploy so busy
// that the shell ran out of its ten seconds) is tried again, but not more
// often than once a minute, so a profile that always hangs costs a builder's
// start ten seconds now and then and never every time.
type pathCache struct {
	mu       sync.Mutex
	text     string
	failedAt time.Time
	read     func() string
	now      func() time.Time
}

// loginPathRetry is how long a failed read stands before the next try.
const loginPathRetry = time.Minute

func (c *pathCache) get() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.text != "" {
		return c.text
	}
	if !c.failedAt.IsZero() && c.now().Sub(c.failedAt) < loginPathRetry {
		return ""
	}
	if c.text = c.read(); c.text == "" {
		c.failedAt = c.now()
	}
	return c.text
}

var loginPaths = &pathCache{read: func() string { return readLoginPath(10 * time.Second) }, now: time.Now}

// loginPath is the PATH a login shell of this user has, read once: a
// profile does not change under a running daemon often enough to ask at
// every start, and asking costs a shell. "" when it cannot be read (no
// bash, a profile that hangs or prints nothing); the session then keeps the
// daemon's PATH, as every session did before, the log says so, and a later
// start asks again.
func loginPath() string { return loginPaths.get() }

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
