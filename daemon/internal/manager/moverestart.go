package manager

import (
	"errors"
	"fmt"
	"time"

	"ccmux.dev/ccmuxd/internal/claudever"
)

// A repo moved to another window is restarted, so its Claude reads the new
// window's sessions, agents and shared folder: the bus gives those once, at
// start (the shim's MCP instructions). A tmux pane is restarted the way the
// update restart does it (autorestart.go): /exit, then its startup command
// with --resume. Unlike that restart it does not wait an hour: the move is
// the person's act, now. But it still only acts on a Claude sitting idle at
// an empty prompt; anything else is left alone and the caller tells it.

// moveQuiet is how long the session's transcript must have been still. A
// turn a bus message starts fires no prompt hook, only transcript writes.
var moveQuiet = 20 * time.Second

// ClaudePanes are a workspace's tmux panes whose foreground is Claude
// (a runner session's pane is not one: it is not in Panes), less a task's
// tmux worker: its task stays in the old window's queue, and an /exit would
// read to the watchdog as the worker quitting.
func (m *Manager) ClaudePanes(wsID string) []string {
	m.mu.RLock()
	defer m.mu.RUnlock()
	e := m.byID[wsID]
	if e == nil {
		return nil
	}
	var out []string
	for _, p := range e.ws.Panes {
		if runsClaude(p) && p.Agent == "" && p.CreatedBy != taskRunner {
			out = append(out, p.ID)
		}
	}
	return out
}

// RestartMoved restarts the Claude in a tmux pane, resuming its session. It
// returns why not, having typed nothing, when the Claude is not plainly
// idle; an error after /exit was typed says so.
func (m *Manager) RestartMoved(paneID string) error {
	extra, err := m.moveRestartable(paneID, time.Now())
	if err != nil {
		return err
	}
	defer m.restart.end(paneID)
	plan, ok := m.restartPlanFor(paneID, extra)
	if !ok {
		return errors.New("its session has no id a relaunch could resume")
	}
	screen, _, err := plan.ctrl.CaptureScreen(paneID)
	if err != nil {
		return fmt.Errorf("its screen could not be read: %w", err)
	}
	if !promptEmpty(screen) {
		return errors.New("its input box is not empty")
	}
	if err := m.typeExit(plan.ctrl, paneID); err != nil {
		return fmt.Errorf("typing /exit failed: %w", err)
	}
	if !m.waitUntil(restartExitWait, func() bool { return m.PaneAtShell(paneID) }) {
		return errors.New("/exit was typed but Claude did not exit")
	}
	m.deliverStartup(plan.ctrl, paneID, plan.cmd, plan.confirm)
	if !m.waitUntil(restartBackWait, func() bool { return m.paneRunsClaude(paneID) }) {
		return errors.New("Claude exited but did not come back: " + plan.cmd)
	}
	return nil
}

// moveRestartable reads the pane, its process and its hooks, and says
// whether a move may restart it now; when it may, the pane is marked in
// flight (the caller ends it) and the flags its relaunch carries come back.
func (m *Manager) moveRestartable(paneID string, now time.Time) ([]string, error) {
	if m.restart.procRoot == "" {
		return nil, errors.New("no process table to read on this host")
	}
	running, err := claudever.Scan(m.restart.procRoot)
	if err != nil {
		return nil, fmt.Errorf("the process table could not be read: %w", err)
	}
	v, extra, ok := m.moveView(paneID, running[paneID])
	if !ok {
		return nil, errors.New("the pane is gone")
	}
	if why := moveBlocker(v, now); why != "" {
		return nil, errors.New(why)
	}
	if !m.restart.tryBegin(paneID) {
		return nil, errors.New("a restart is already running in it")
	}
	return extra, nil
}

// moveView is restartView for a move: behind is not a version here but
// always true, and failed is not read (a failed update restart says nothing
// about this one).
func (m *Manager) moveView(paneID string, proc claudever.Proc) (restartView, []string, bool) {
	m.mu.RLock()
	_, p := m.findPaneLocked(paneID)
	if p == nil {
		m.mu.RUnlock()
		return restartView{}, nil, false
	}
	startup := p.StartupCommand
	eligible := p.Agent == "" && p.HostedClaude && !p.DevServer && relaunchable(startup)
	claude := runsClaude(p)
	m.mu.RUnlock()
	extra, matches := relaunchFlags(startup, proc.Argv)
	clock, known := m.restart.clocks.get(paneID)
	_, inflight := m.restart.state(paneID, "")
	v := restartView{
		behind: true, eligible: eligible && matches, runsClaude: claude, known: known, clock: clock,
		background: proc.Background, inflight: inflight,
	}
	if known && clock.session != "" {
		v.transcript = transcriptWrite(m.restart.claudeDir, clock.session)
	}
	return v, extra, true
}

// moveBlocker is the first reason a move may not restart the pane, "" when
// it may.
func moveBlocker(v restartView, now time.Time) string {
	switch {
	case !v.runsClaude:
		return "no Claude runs in it"
	case !v.eligible:
		return "its startup command is not one ccmux can relaunch"
	case v.background:
		return "it has a background task that /exit would end"
	case v.inflight:
		return "a restart is already running in it"
	case !v.clockReady():
		return "it is working or waiting on a question"
	case v.quietFor(now) < moveQuiet:
		return "it was active a moment ago, or its transcript was not found"
	}
	return ""
}

// paneRunsClaude reports that Claude is the pane's foreground again.
func (m *Manager) paneRunsClaude(paneID string) bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	_, p := m.findPaneLocked(paneID)
	return p != nil && runsClaude(p)
}

// tryBegin marks a pane's restart in flight unless one already is.
func (r *autoRestart) tryBegin(pane string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.inflight[pane] {
		return false
	}
	if r.inflight == nil {
		r.inflight = map[string]bool{}
	}
	r.inflight[pane] = true
	return true
}
