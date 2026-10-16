package manager

import (
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"ccmux.dev/ccmuxd/internal/claudever"
	"ccmux.dev/ccmuxd/internal/model"
	"ccmux.dev/ccmuxd/internal/session"
)

// restartIdleWait is how long a pane sits idle before ccmux restarts it onto
// a newer Claude Code (see autorestart_clock.go for why ccmux does this).
const restartIdleWait = time.Hour

// Each wait is generous: a miss marks the pane failed for this version, and a
// slow machine should not be the reason. Variables so a test can wait less.
var (
	restartExitWait = 30 * time.Second
	restartBackWait = 90 * time.Second
	restartPoll     = time.Second
)

// autoRestart is the manager's restart state: the hook-driven clocks, the
// panes a restart is running on, and the version each failed pane failed to
// reach (it is not retried until a newer one is installed).
type autoRestart struct {
	clocks    restartClocks
	link      string // the launcher link naming the installed version
	procRoot  string
	claudeDir string // where Claude keeps transcripts (projects/*/<id>.jsonl)
	warned    bool   // the link was unreadable and that has been logged

	mu       sync.Mutex
	failed   map[string]string
	inflight map[string]bool
	noted    map[string]string // pane -> the last skip reason logged for it
}

func (r *autoRestart) begin(pane string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.inflight == nil {
		r.inflight = map[string]bool{}
	}
	r.inflight[pane] = true
}

func (r *autoRestart) end(pane string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.inflight, pane)
}

// resolve forgets a failure once the pane is seen on the installed version:
// fixed by hand, it must not show "failed" the next time it sits at a shell.
func (r *autoRestart) resolve(pane string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.failed, pane)
}

// note logs why a pending pane was skipped, once per reason and version, so
// a pane the tab says will restart, and that never does, is explained in the
// log rather than only by its silence.
func (r *autoRestart) note(pane, version, reason string) {
	key := version + ": " + reason
	r.mu.Lock()
	if r.noted == nil {
		r.noted = map[string]string{}
	}
	seen := r.noted[pane] == key
	r.noted[pane] = key
	r.mu.Unlock()
	if !seen {
		log.Printf("pane %s: Claude auto restart to %s waits: %s", pane, version, reason)
	}
}

func (r *autoRestart) fail(pane, version string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.failed == nil {
		r.failed = map[string]string{}
	}
	r.failed[pane] = version
}

// state reads a pane's flags: whether it failed on this version, whether a
// restart is running on it now.
func (r *autoRestart) state(pane, version string) (failed, inflight bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.failed[pane] == version, r.inflight[pane]
}

func (r *autoRestart) forget(pane string) {
	r.clocks.forget(pane)
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.failed, pane)
	delete(r.noted, pane)
}

// StartAutoRestart checks every pane each tick against the version link
// names (normally ~/.local/bin/claude). Linux only, like the listener scan:
// without a proc tree no pane ever reads as behind and nothing happens.
func (m *Manager) StartAutoRestart(every time.Duration, link string) {
	m.restart.link, m.restart.procRoot, m.restart.claudeDir = link, "/proc", claudeConfigDir()
	go func() {
		t := time.NewTicker(every)
		defer t.Stop()
		for {
			select {
			case <-m.ctx.Done():
				return
			case now := <-t.C:
				m.restartTick(now)
			}
		}
	}()
}

// claudeConfigDir is where Claude Code keeps its state, honouring the same
// override Claude does.
func claudeConfigDir() string {
	if d := os.Getenv("CLAUDE_CONFIG_DIR"); d != "" {
		return d
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".claude")
}

func (m *Manager) restartTick(now time.Time) {
	installed, err := claudever.Installed(m.restart.link)
	if err != nil {
		if !m.restart.warned {
			log.Printf("claude auto restart off until the launcher link reads: %v", err)
			m.restart.warned = true
		}
		return
	}
	m.restart.warned = false
	running, err := claudever.Scan(m.restart.procRoot)
	if err != nil {
		return
	}
	for _, tg := range m.markRestarts(installed, running, now) {
		go m.restartPane(tg, installed)
	}
}

// restartTarget is a pane due a restart, with the carried flags its running
// Claude needs on the relaunch (relaunchFlags).
type restartTarget struct {
	paneID, wsID string
	extra        []string
}

// markRestarts sets every live pane's ClaudeUpdate, broadcasts each
// workspace whose marks changed, and returns the panes due a restart, already
// marked in flight so the next tick leaves them alone.
func (m *Manager) markRestarts(installed string, running map[string]claudever.Proc, now time.Time) []restartTarget {
	var due []restartTarget
	changed := map[string]bool{}
	m.mu.Lock()
	for _, e := range m.byID {
		if e.ctrl == nil {
			continue
		}
		for _, p := range e.ws.Panes {
			v, extra := m.restartViewFor(p, installed, running[p.ID])
			if mark := restartMark(v); mark != p.ClaudeUpdate {
				p.ClaudeUpdate = mark
				changed[e.ws.ID] = true
			}
			m.noteNoTranscript(p.ID, installed, v)
			if decideRestart(v, now, restartIdleWait) {
				m.restart.begin(p.ID)
				due = append(due, restartTarget{p.ID, e.ws.ID, extra})
			}
		}
	}
	m.mu.Unlock()
	for wsID := range changed {
		m.events.publish(Event{Kind: "workspace-status", WorkspaceID: wsID})
	}
	return due
}

// restartViewFor reads one pane for the rule, and the carried flags its
// relaunch would append.
func (m *Manager) restartViewFor(p *model.Pane, installed string, proc claudever.Proc) (restartView, []string) {
	if proc.Version != "" && !claudever.Older(proc.Version, installed) {
		m.restart.resolve(p.ID)
	}
	clock, known := m.restart.clocks.get(p.ID)
	failed, inflight := m.restart.state(p.ID, installed)
	extra, matches := relaunchFlags(p.StartupCommand, proc.Argv)
	behind := claudever.Older(proc.Version, installed)
	var transcript time.Time
	if behind { // a glob over every project per pane per minute only where it can matter
		transcript = transcriptWrite(m.restart.claudeDir, clock.session)
	}
	return restartView{
		behind:     behind,
		eligible:   p.Agent == "" && p.HostedClaude && !p.DevServer && relaunchable(p.StartupCommand) && matches,
		runsClaude: runsClaude(p),
		known:      known,
		clock:      clock,
		transcript: transcript,
		background: proc.Background,
		failed:     failed,
		inflight:   inflight,
	}, extra
}

// noteNoTranscript explains the wait quietFor makes until a transcript
// appears: a session with none where the daemon looks (brand new, or kept
// under another CLAUDE_CONFIG_DIR). The tab says "manual" meanwhile.
func (m *Manager) noteNoTranscript(paneID, installed string, v restartView) {
	if v.ready() && v.clockReady() && v.transcript.IsZero() {
		m.restart.note(paneID, installed, "no transcript for session "+v.clock.session+" under "+m.restart.claudeDir)
	}
}

// restartPlan is what a restart needs, read under the lock in one go.
type restartPlan struct {
	ctrl    *session.Controller
	cmd     string // the startup command with --resume <session>; there is no relaunch without one
	confirm bool
}

func (m *Manager) restartPlanFor(paneID string, extra []string) (restartPlan, bool) {
	clock, _ := m.restart.clocks.get(paneID)
	m.mu.RLock()
	defer m.mu.RUnlock()
	e, p := m.findPaneLocked(paneID)
	if p == nil || e.ctrl == nil {
		return restartPlan{}, false
	}
	cmd, ok := resumeCommand(strings.Join(append([]string{p.StartupCommand}, extra...), " "), clock.session)
	if !ok {
		return restartPlan{}, false
	}
	return restartPlan{ctrl: e.ctrl, cmd: cmd, confirm: m.confirmFor(p)}, true
}

// restartPane exits the pane's Claude and starts it again on the installed
// version. It looks at the screen first and leaves a pane alone whose input
// box is not plainly empty: the next tick tries again. Once /exit is typed,
// any miss marks the pane failed for this version.
func (m *Manager) restartPane(tg restartTarget, installed string) {
	defer m.restart.end(tg.paneID)
	plan, ok := m.restartPlanFor(tg.paneID, tg.extra)
	if !ok {
		return
	}
	screen, _, err := plan.ctrl.CaptureScreen(tg.paneID)
	if err != nil {
		m.restart.note(tg.paneID, installed, "screen capture failed: "+err.Error())
		return
	}
	if !promptEmpty(screen) {
		m.restart.note(tg.paneID, installed, "the input box is not empty, or not one this check recognizes")
		return
	}
	log.Printf("pane %s: Claude is behind %s and idle %s, restarting it: %s", tg.paneID, installed, restartIdleWait, plan.cmd)
	if err := m.typeExit(plan.ctrl, tg.paneID); err != nil {
		m.restartFailed(tg.paneID, installed, "typing /exit: "+err.Error())
		return
	}
	if !m.waitUntil(restartExitWait, func() bool { return m.PaneAtShell(tg.paneID) }) {
		m.restartFailed(tg.paneID, installed, "Claude did not exit")
		return
	}
	m.deliverStartup(plan.ctrl, tg.paneID, plan.cmd, plan.confirm)
	if !m.waitUntil(restartBackWait, func() bool { return m.runsInstalled(tg.paneID, installed) }) {
		m.restartFailed(tg.paneID, installed, "Claude did not come back on "+installed)
		return
	}
	log.Printf("pane %s: Claude restarted on %s", tg.paneID, installed)
}

// typeExit types /exit and, after a beat, Enter: sent as one burst, Claude
// can read the pair as a paste and leave the command in the box.
func (m *Manager) typeExit(ctrl *session.Controller, paneID string) error {
	if err := ctrl.SendInput(paneID, []byte("/exit")); err != nil {
		return err
	}
	select {
	case <-m.ctx.Done():
		return m.ctx.Err()
	case <-time.After(300 * time.Millisecond):
	}
	return ctrl.SendInput(paneID, []byte("\r"))
}

func (m *Manager) runsInstalled(paneID, installed string) bool {
	running, err := claudever.Scan(m.restart.procRoot)
	if err != nil {
		return false
	}
	v := running[paneID].Version
	return v != "" && !claudever.Older(v, installed)
}

func (m *Manager) restartFailed(paneID, installed, why string) {
	log.Printf("pane %s: Claude auto restart to %s failed, not retrying this version: %s", paneID, installed, why)
	m.restart.fail(paneID, installed)
}

// waitUntil polls cond until it holds, the limit passes, or the daemon stops.
func (m *Manager) waitUntil(limit time.Duration, cond func() bool) bool {
	deadline := time.Now().Add(limit)
	for !cond() {
		if time.Now().After(deadline) {
			return false
		}
		select {
		case <-m.ctx.Done():
			return false
		case <-time.After(restartPoll):
		}
	}
	return true
}
