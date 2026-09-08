package api

// The scheduler tick: which due schedules fire, and how the prompt reaches
// the instance. Split from the routes so each side stays readable.

import (
	"errors"
	"fmt"
	"log"
	"net/http"
	"time"

	"ccmux.dev/ccmuxd/internal/agent"
	"ccmux.dev/ccmuxd/internal/manager"
	"ccmux.dev/ccmuxd/internal/schedule"
	"ccmux.dev/ccmuxd/internal/store"
)

// fireAction is what one tick decides for one due schedule.
type fireAction int

const (
	fireStart fireAction = iota // asleep or absent: start it with the prompt as first line
	firePush                    // running opencode, idle: push into its TUI
	fireWait                    // running and busy, or a start in flight: try next tick
	fireSkip                    // running Claude harness: no chat path, skip this slot
)

func (s *Server) fireDueSchedules() {
	if s.schedules == nil || s.agents == nil {
		return
	}
	now := s.scheduleNow()
	due, err := s.schedules.DueAgentSchedules(now.UnixMilli())
	if err != nil {
		log.Printf("schedules: reading due rows: %v", err)
		return
	}
	for _, sc := range due {
		s.fireSchedule(sc, now)
	}
}

// fireSchedule delivers one due schedule, or decides not to: a slot missed
// by more than the catch-up window is skipped, a busy agent is retried next
// tick, a running Claude instance is skipped and logged.
func (s *Server) fireSchedule(sc store.AgentSchedule, now time.Time) {
	spec, err := schedule.Parse(sc.Cron)
	if err != nil {
		log.Printf("agent %s: schedule #%d has an unreadable cron line (%q); paused", sc.Agent, sc.ID, sc.Cron)
		_, _ = s.schedules.SetAgentSchedulePaused(sc.ID, true, sc.NextRunAt)
		return
	}
	next, hasNext := nextRunMillis(spec, now)
	if !hasNext {
		log.Printf("agent %s: schedule #%d never fires again (%q); paused", sc.Agent, sc.ID, sc.Cron)
		_, _ = s.schedules.SetAgentSchedulePaused(sc.ID, true, sc.NextRunAt)
		return
	}
	win, d, ok := s.scheduleTarget(sc)
	if !ok {
		return
	}
	if late := now.Sub(time.UnixMilli(sc.NextRunAt)); late > s.scheduleCatchUp {
		log.Printf("agent %s: schedule #%d missed its slot by %s (more than %s); skipping to %s", sc.Agent, sc.ID, late.Round(time.Minute), s.scheduleCatchUp, viewOf(store.AgentSchedule{NextRunAt: next}).NextRun)
		_ = s.schedules.MarkAgentScheduleRun(sc.ID, 0, next)
		return
	}
	text := fmt.Sprintf("[ccmux schedule #%d, %s] %s", sc.ID, sc.Cron, sc.Prompt)
	action, paneID := s.decideFire(win, d)
	if action == fireSkip {
		log.Printf("agent %s in %s: schedule #%d due while a Claude-harness instance runs; no chat path, skipping to %s", sc.Agent, win.Name, sc.ID, viewOf(store.AgentSchedule{NextRunAt: next}).NextRun)
		_ = s.schedules.MarkAgentScheduleRun(sc.ID, 0, next)
		return
	}
	delivered, err := s.deliverSchedule(action, paneID, win, sc.Agent, text)
	if err != nil {
		log.Printf("agent %s in %s: schedule #%d not delivered (%v); retrying next tick", sc.Agent, win.Name, sc.ID, err)
	}
	if !delivered {
		return // busy, a start in flight, or the error above: the next tick retries, bounded by the catch-up window
	}
	log.Printf("agent %s in %s: schedule #%d fired", sc.Agent, win.Name, sc.ID)
	_ = s.schedules.MarkAgentScheduleRun(sc.ID, now.UnixMilli(), next)
}

// deliverSchedule hands the text to the instance per the decided action.
// false without an error is a wait (busy, or another start in flight).
func (s *Server) deliverSchedule(action fireAction, paneID string, win manager.WindowInfo, name, text string) (bool, error) {
	switch action {
	case firePush:
		if err := s.mgr.PushToAgentPane(paneID, text); err != nil {
			return false, err
		}
		return true, nil
	case fireStart:
		out := s.startWindowAgent(win, name, text, scheduleSource, nil)
		if errors.Is(out.err, manager.ErrAgentRunning) {
			return false, nil // its TUI takes a push next tick
		}
		if out.msg != "" {
			return false, errors.New(out.msg)
		}
		return true, nil
	}
	return false, nil
}

// scheduleTarget resolves the window and base a schedule points at. A
// window or base that no longer exists takes the schedule with it.
func (s *Server) scheduleTarget(sc store.AgentSchedule) (manager.WindowInfo, agent.Definition, bool) {
	win, status, msg := s.windowByID(sc.WindowID)
	if status == http.StatusNotFound {
		log.Printf("agent %s: schedule #%d points at a window that is gone; removed", sc.Agent, sc.ID)
		_, _ = s.schedules.DeleteAgentSchedule(sc.ID)
		return win, agent.Definition{}, false
	}
	if msg != "" {
		log.Printf("agent %s: schedule #%d skipped this tick: %s", sc.Agent, sc.ID, msg)
		return win, agent.Definition{}, false
	}
	d, err := s.agents.Get(sc.Agent)
	if errors.Is(err, agent.ErrNotFound) {
		log.Printf("agent %s: schedule #%d points at a deleted base; removed", sc.Agent, sc.ID)
		_, _ = s.schedules.DeleteAgentSchedule(sc.ID)
		return win, d, false
	}
	if err != nil {
		log.Printf("agent %s: schedule #%d skipped this tick: %v", sc.Agent, sc.ID, err)
		return win, d, false
	}
	return win, d, true
}

// decideFire reads the instance's state: not running means start it with
// the prompt; running opencode means push when idle and wait when busy;
// running Claude has no chat path and is skipped.
func (s *Server) decideFire(win manager.WindowInfo, d agent.Definition) (fireAction, string) {
	inst := s.instanceOf(win, d)
	if inst.State != "running" {
		return fireStart, ""
	}
	p := s.mgr.AgentPane(inst.Workspace, d.Name)
	if p == nil || agent.OpencodePort(p.StartupCommand) == 0 {
		return fireSkip, ""
	}
	if s.mgr.AgentBusy(p.ID) {
		return fireWait, ""
	}
	return firePush, p.ID
}
