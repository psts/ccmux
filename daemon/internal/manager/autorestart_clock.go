package manager

import (
	"sync"
	"time"

	"ccmux.dev/ccmuxd/internal/model"
)

// Auto restart of Claude panes after a Claude Code update. The update lands
// on disk while every session keeps running the old version; Claude's own
// /restart re-execs without --dangerously-load-development-channels, which
// silently drops the pane from push delivery on the peers bus. So ccmux does
// the restart itself: once a pane has sat idle for an hour it types /exit and
// then the pane's own startup command with --resume, which keeps both the
// conversation and the flag.
//
// This file is the pure part: the per-pane clock the hooks drive, and the
// rule that reads it. autorestart.go runs it.

// restartClock is what the hooks have said about one pane's Claude.
type restartClock struct {
	session   string    // the running session's id, "" when unknown
	idleSince time.Time // when the last turn ended; zero while one runs
	blocked   bool      // a permission prompt or a question is open
}

// restartClocks holds every pane's clock. In memory only: after a daemon
// restart a pane waits for its next hook, which at worst delays one restart.
type restartClocks struct {
	mu sync.Mutex
	m  map[string]*restartClock
}

func (c *restartClocks) at(pane string) *restartClock {
	if c.m == nil {
		c.m = map[string]*restartClock{}
	}
	k := c.m[pane]
	if k == nil {
		k = &restartClock{}
		c.m[pane] = k
	}
	return k
}

// attention notes a hook's attention for the pane: a Stop starts the idle
// clock, a permission prompt or question blocks, anything else (a prompt
// submitted, a held Stop while subagents run) means a turn is running.
func (c *restartClocks) attention(pane string, att model.Attention, reason model.AttentionReason, now time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	k := c.at(pane)
	switch {
	case att == model.AttentionDone, att == model.AttentionNeedsInput && reason == model.ReasonFinished:
		k.idleSince, k.blocked = now, false
	case att == model.AttentionNeedsInput:
		k.blocked = true
	default:
		k.idleSince, k.blocked = time.Time{}, false
	}
}

// reminder is Claude's idle_prompt, which only comes while the session sits
// at its prompt. It starts the clock for a session no Stop ever ended (a
// fresh one, a /clear) and leaves a running clock alone.
func (c *restartClocks) reminder(pane string, now time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	k := c.at(pane)
	if !k.blocked && k.idleSince.IsZero() {
		k.idleSince = now
	}
}

// session notes which session runs in the pane. Only its own end clears the
// id, so a late end from the session a /clear replaced changes nothing.
func (c *restartClocks) session(pane, id string, sig model.SessionSignal) {
	c.mu.Lock()
	defer c.mu.Unlock()
	k := c.at(pane)
	switch sig {
	case model.SessionStarted:
		*k = restartClock{session: id}
	case model.SessionActive:
		if k.session == "" {
			k.session = id
		}
	case model.SessionEnded:
		if k.session == id {
			k.session = ""
		}
	case model.SessionNone:
		k.session = ""
	}
}

func (c *restartClocks) get(pane string) (restartClock, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	k, ok := c.m[pane]
	if !ok {
		return restartClock{}, false
	}
	return *k, true
}

func (c *restartClocks) forget(pane string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.m, pane)
}

// restartView is the slice of pane state the rule reads.
type restartView struct {
	behind     bool // its Claude runs a version older than the installed one
	eligible   bool // a plain Claude pane whose startup command we can relaunch
	runsClaude bool // Claude is the pane's foreground now
	known      bool // some hook has spoken for this pane since the daemon started
	clock      restartClock
	transcript time.Time // the session transcript's last write; zero when none was found
	background bool      // its Claude still runs a Bash tool command (a dev server, a watch)
	failed     bool      // a restart to the installed version already failed here
	inflight   bool      // a restart is running now
}

// decideRestart is the whole rule: behind, ours to relaunch, at Claude's
// prompt with a known session, no card open and no background task, quiet at
// least wait, and not already tried for this version.
func decideRestart(v restartView, now time.Time, wait time.Duration) bool {
	return v.ready() && v.clockReady() && v.quietFor(now) >= wait
}

// quietFor is how long nothing has happened in the session: since the later
// of its last Stop and its transcript's last write. The hooks alone cannot
// say a turn started: one a bus message or a task wake begins fires no
// user_prompt_submit (1107 of 1897 Stops in the hook trace on 2026-10-16
// had none since the Stop before). Claude writes the transcript on every
// message, those included. A session whose transcript cannot be found is
// never quiet: nothing proves it idle, and nothing proves a relaunch could
// resume it. A brand-new session waits for its first message to write one.
func (v restartView) quietFor(now time.Time) time.Duration {
	if v.transcript.IsZero() {
		return 0
	}
	last := v.clock.idleSince
	if v.transcript.After(last) {
		last = v.transcript
	}
	return now.Sub(last)
}

// ready is everything the rule reads from the pane and the processes.
func (v restartView) ready() bool {
	return v.behind && v.eligible && v.runsClaude && !v.background && !v.failed && !v.inflight
}

// clockReady is everything it reads from the hooks.
func (v restartView) clockReady() bool {
	k := v.clock
	return v.known && k.session != "" && !k.blocked && !k.idleSince.IsZero()
}

// restartMark is what the lenses show on the pane's tab: "pending" for a
// pane ccmux will restart, "manual" for one it cannot, which the human has to
// restart by hand. Manual is a restart that failed, held while the pane is
// still behind or has no Claude at all (the relaunch never came up) and gone
// once a Claude runs on the installed version; or a known session whose
// transcript is not where the daemon looks, so a restart could not resume
// it. A pane that is behind but not ours to restart (an agent, which its own
// lifecycle restarts, or an odd startup command) shows nothing.
func restartMark(v restartView) string {
	switch {
	case v.failed && (v.behind || !v.runsClaude):
		return "manual"
	case !v.behind || !v.eligible:
		return ""
	case v.known && v.clock.session != "" && v.transcript.IsZero():
		return "manual"
	default:
		return "pending"
	}
}
