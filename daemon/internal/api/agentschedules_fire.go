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
	fireWait                    // running and busy: try next tick
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
		s.pauseBroken(sc, "has an unreadable cron line")
		return
	}
	next, hasNext := nextRunMillis(spec, now)
	if !hasNext {
		s.pauseBroken(sc, "never fires again")
		return
	}
	win, d, ok := s.scheduleTarget(sc)
	if !ok {
		return
	}
	if late := now.Sub(time.UnixMilli(sc.NextRunAt)); late > s.scheduleCatchUp {
		log.Printf("agent %s: schedule #%d missed its slot by %s (more than %s); skipping to %s", sc.Agent, sc.ID, late.Round(time.Minute), s.scheduleCatchUp, nextRunText(next))
		s.markRun(sc, 0, next)
		return
	}
	text := fmt.Sprintf("[ccmux schedule #%d, %s] %s", sc.ID, sc.Cron, sc.Prompt)
	action, paneID := s.decideFire(win, d)
	if action == fireSkip {
		log.Printf("agent %s in %s: schedule #%d due while a Claude-harness instance runs; no chat path, skipping to %s", sc.Agent, win.Name, sc.ID, nextRunText(next))
		s.markRun(sc, 0, next)
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
	s.markRun(sc, now.UnixMilli(), next)
}

// markRun records a run (or a skip, ranAt 0) and moves the row to its next
// slot. A failed write is the one thing that must not stay quiet: the row
// would still read as due and the same prompt would go out every tick, so
// the schedule is paused and the log says why.
func (s *Server) markRun(sc store.AgentSchedule, ranAt, next int64) {
	err := s.schedules.MarkAgentScheduleRun(sc.ID, ranAt, next)
	if err == nil {
		return
	}
	log.Printf("agent %s: schedule #%d ran but its next slot could not be saved (%v); pausing it so it does not repeat every tick", sc.Agent, sc.ID, err)
	if _, perr := s.schedules.SetAgentSchedulePaused(sc.ID, true, sc.NextRunAt); perr != nil {
		log.Printf("agent %s: schedule #%d could not be paused either: %v", sc.Agent, sc.ID, perr)
	}
}

// pauseBroken parks a schedule whose cron line cannot produce a run.
func (s *Server) pauseBroken(sc store.AgentSchedule, why string) {
	if _, err := s.schedules.SetAgentSchedulePaused(sc.ID, true, sc.NextRunAt); err != nil {
		log.Printf("agent %s: schedule #%d %s (%q) and could not be paused: %v", sc.Agent, sc.ID, why, sc.Cron, err)
		return
	}
	log.Printf("agent %s: schedule #%d %s (%q); paused", sc.Agent, sc.ID, why, sc.Cron)
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
		s.dropOrphan(sc, "a window that is gone")
		return win, agent.Definition{}, false
	}
	if msg != "" {
		log.Printf("agent %s: schedule #%d skipped this tick: %s", sc.Agent, sc.ID, msg)
		return win, agent.Definition{}, false
	}
	d, err := s.agents.Get(sc.Agent)
	if errors.Is(err, agent.ErrNotFound) {
		s.dropOrphan(sc, "a deleted base")
		return win, d, false
	}
	if err != nil {
		log.Printf("agent %s: schedule #%d skipped this tick: %v", sc.Agent, sc.ID, err)
		return win, d, false
	}
	return win, d, true
}

// dropOrphan removes a schedule whose target no longer exists.
func (s *Server) dropOrphan(sc store.AgentSchedule, what string) {
	if _, err := s.schedules.DeleteAgentSchedule(sc.ID); err != nil {
		log.Printf("agent %s: schedule #%d points at %s and could not be removed: %v", sc.Agent, sc.ID, what, err)
		return
	}
	log.Printf("agent %s: schedule #%d points at %s; removed", sc.Agent, sc.ID, what)
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
