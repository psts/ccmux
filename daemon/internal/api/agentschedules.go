package api

// Timed prompts for agent instances (docs/agent-spec.md §8, "Schedules").
//
// An agent asks for one from its own chat through the peers shim's
// `schedule` tool, which posts here with the pane's identity; the lenses'
// chat header uses the same pane route, and the agent editor reads a count
// per instance through the window-scoped list. The daemon's
// tick hands a due prompt to the instance the way a chat message would:
// pushed into a running opencode TUI, or typed as the first line of a
// start when the instance is asleep. There is no cron daemon under this;
// next_run_at is stored, and the tick compares it with the clock.

import (
	"context"
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

// scheduleStore is the slice of the registry the scheduler needs.
type scheduleStore interface {
	AddAgentSchedule(store.AgentSchedule) (int64, error)
	AgentSchedule(id int64) (*store.AgentSchedule, error)
	AgentSchedulesFor(windowID, agent string) ([]store.AgentSchedule, error)
	DueAgentSchedules(nowMillis int64) ([]store.AgentSchedule, error)
	SetAgentSchedulePaused(id int64, paused bool, nextRun int64) (bool, error)
	MarkAgentScheduleRun(id int64, ranAt, nextRun int64) error
	DeleteAgentSchedule(id int64) (bool, error)
}

const (
	// maxSchedulesPerInstance keeps a runaway agent from filling the table.
	maxSchedulesPerInstance = 20
	maxSchedulePrompt       = 4000
	// scheduleSource is the createdBy stamp on a session a schedule started.
	scheduleSource = "scheduler"
)

// SetSchedules wires the store and the catch-up window: a run due longer
// ago than catchUp (the daemon was down, or the agent stayed busy) is
// skipped to its next slot instead of firing late.
func (s *Server) SetSchedules(st scheduleStore, catchUp time.Duration) {
	s.schedules, s.scheduleCatchUp = st, catchUp
	if s.scheduleNow == nil {
		s.scheduleNow = time.Now
	}
}

// StartScheduler ticks the due schedules every `every` for the life of ctx.
func (s *Server) StartScheduler(ctx context.Context, every time.Duration) {
	if s.schedules == nil {
		return
	}
	go func() {
		t := time.NewTicker(every)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				s.fireDueSchedules()
			}
		}
	}()
}

// scheduleView is a schedule on the wire, with the next run spelled out in
// the daemon's local time so an agent can echo it back to the human.
type scheduleView struct {
	store.AgentSchedule
	NextRun string `json:"nextRun"`
}

func viewOf(sc store.AgentSchedule) scheduleView {
	v := scheduleView{AgentSchedule: sc}
	if !sc.Paused && sc.NextRunAt > 0 {
		v.NextRun = time.UnixMilli(sc.NextRunAt).Local().Format("Mon 2 Jan 15:04 MST")
	}
	return v
}

func viewsOf(list []store.AgentSchedule) []scheduleView {
	out := make([]scheduleView, 0, len(list))
	for _, sc := range list {
		out = append(out, viewOf(sc))
	}
	return out
}

// nextRunMillis is the schedule's next slot after now, in the local zone.
func nextRunMillis(spec schedule.Spec, now time.Time) (int64, bool) {
	next, ok := spec.Next(now.Local())
	if !ok {
		return 0, false
	}
	return next.UnixMilli(), true
}

// --- the pane route: what the shim's `schedule` tool calls ---

type scheduleReq struct {
	Action string `json:"action"`
	Cron   string `json:"cron"`
	Prompt string `json:"prompt"`
	ID     int64  `json:"id"`
}

// paneSchedules: POST /v1/panes/{id}/schedules {action, cron, prompt, id}
// from an agent pane. Answers {message, schedules} for every action so the
// tool can show the list after a change.
func (s *Server) paneSchedules(w http.ResponseWriter, r *http.Request) {
	if s.schedules == nil {
		writeError(w, http.StatusServiceUnavailable, "schedules are not available on this daemon")
		return
	}
	win, name, status, msg := s.instanceOfPane(r.PathValue("id"))
	if msg != "" {
		writeError(w, status, msg)
		return
	}
	var req scheduleReq
	if !decodeJSON(w, r, &req) {
		return
	}
	message, status, err := s.applyScheduleAction(win.ID, name, r.PathValue("id"), req)
	if err != nil {
		writeError(w, status, err.Error())
		return
	}
	list, err := s.schedules.AgentSchedulesFor(win.ID, name)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"message": message, "schedules": viewsOf(list)})
}

// instanceOfPane names the window and agent an agent pane belongs to.
func (s *Server) instanceOfPane(paneID string) (manager.WindowInfo, string, int, string) {
	ws := s.mgr.Workspace(s.mgr.WorkspaceForPane(paneID))
	if ws == nil {
		return manager.WindowInfo{}, "", http.StatusNotFound, "unknown pane"
	}
	name := ""
	for _, p := range ws.Panes {
		if p.ID == paneID {
			name = p.Agent
		}
	}
	if name == "" {
		return manager.WindowInfo{}, "", http.StatusBadRequest, "not an agent pane: only an agent can schedule its own runs"
	}
	group, ok := s.mgr.GroupForPane(paneID)
	if !ok || group == "" {
		return manager.WindowInfo{}, "", http.StatusBadRequest, "this agent is in no shared window, so it has nothing to schedule against"
	}
	win, status, msg := s.windowByName(group)
	return win, name, status, msg
}

func (s *Server) applyScheduleAction(windowID, name, paneID string, req scheduleReq) (string, int, error) {
	switch req.Action {
	case "add":
		return s.addSchedule(windowID, name, paneID, req)
	case "list":
		return "", 0, nil
	case "pause", "resume":
		return s.pauseSchedule(windowID, name, req.ID, req.Action == "pause")
	case "remove":
		return s.removeSchedule(windowID, name, req.ID)
	}
	return "", http.StatusBadRequest, errors.New(`action must be one of add, list, pause, resume, remove`)
}

func (s *Server) addSchedule(windowID, name, paneID string, req scheduleReq) (string, int, error) {
	spec, err := schedule.Parse(req.Cron)
	if err != nil {
		return "", http.StatusBadRequest, fmt.Errorf("cron: %w", err)
	}
	if req.Prompt == "" || len(req.Prompt) > maxSchedulePrompt {
		return "", http.StatusBadRequest, fmt.Errorf("prompt must be 1 to %d characters", maxSchedulePrompt)
	}
	now := s.scheduleNow()
	next, ok := nextRunMillis(spec, now)
	if !ok {
		return "", http.StatusBadRequest, errors.New("that cron line never fires (a date that does not exist?)")
	}
	existing, err := s.schedules.AgentSchedulesFor(windowID, name)
	if err != nil {
		return "", http.StatusInternalServerError, err
	}
	if len(existing) >= maxSchedulesPerInstance {
		return "", http.StatusConflict, fmt.Errorf("this agent already has %d schedules here; remove one first", maxSchedulesPerInstance)
	}
	id, err := s.schedules.AddAgentSchedule(store.AgentSchedule{
		WindowID: windowID, Agent: name, Cron: req.Cron, Prompt: req.Prompt,
		CreatedBy: paneID, CreatedAt: now.UnixMilli(), NextRunAt: next,
	})
	if err != nil {
		return "", http.StatusInternalServerError, err
	}
	log.Printf("agent %s: schedule #%d added (%s)", name, id, req.Cron)
	return fmt.Sprintf("Scheduled #%d (%s). Next run %s.", id, req.Cron, viewOf(store.AgentSchedule{NextRunAt: next}).NextRun), 0, nil
}

// ownedSchedule loads a schedule and checks it belongs to this instance;
// another instance's id is a 404, never a peek at someone else's row.
func (s *Server) ownedSchedule(windowID, name string, id int64) (*store.AgentSchedule, int, error) {
	sc, err := s.schedules.AgentSchedule(id)
	if err != nil {
		return nil, http.StatusInternalServerError, err
	}
	if sc == nil || sc.WindowID != windowID || sc.Agent != name {
		return nil, http.StatusNotFound, fmt.Errorf("no schedule #%d for %s here", id, name)
	}
	return sc, 0, nil
}

func (s *Server) pauseSchedule(windowID, name string, id int64, paused bool) (string, int, error) {
	sc, status, err := s.ownedSchedule(windowID, name, id)
	if err != nil {
		return "", status, err
	}
	next := sc.NextRunAt
	if !paused {
		// Resume from now: a slot that passed while paused does not fire.
		spec, perr := schedule.Parse(sc.Cron)
		if perr != nil {
			return "", http.StatusInternalServerError, perr
		}
		var ok bool
		if next, ok = nextRunMillis(spec, s.scheduleNow()); !ok {
			return "", http.StatusBadRequest, errors.New("that cron line never fires again; remove it")
		}
	}
	if _, err := s.schedules.SetAgentSchedulePaused(id, paused, next); err != nil {
		return "", http.StatusInternalServerError, err
	}
	if paused {
		return fmt.Sprintf("Paused #%d.", id), 0, nil
	}
	return fmt.Sprintf("Resumed #%d. Next run %s.", id, viewOf(store.AgentSchedule{NextRunAt: next}).NextRun), 0, nil
}

func (s *Server) removeSchedule(windowID, name string, id int64) (string, int, error) {
	if _, status, err := s.ownedSchedule(windowID, name, id); err != nil {
		return "", status, err
	}
	if _, err := s.schedules.DeleteAgentSchedule(id); err != nil {
		return "", http.StatusInternalServerError, err
	}
	log.Printf("agent %s: schedule #%d removed", name, id)
	return fmt.Sprintf("Removed #%d.", id), 0, nil
}

// --- the window routes: what the lenses call ---

// windowSchedulesTarget resolves {id}/{name} of the window routes.
func (s *Server) windowSchedulesTarget(w http.ResponseWriter, r *http.Request) (manager.WindowInfo, string, bool) {
	if s.schedules == nil {
		writeError(w, http.StatusServiceUnavailable, "schedules are not available on this daemon")
		return manager.WindowInfo{}, "", false
	}
	win, status, msg := s.windowByID(r.PathValue("id"))
	if msg != "" {
		writeError(w, status, msg)
		return manager.WindowInfo{}, "", false
	}
	name := r.PathValue("name")
	if !agent.ValidName(name) {
		writeError(w, http.StatusBadRequest, "bad agent name")
		return manager.WindowInfo{}, "", false
	}
	return win, name, true
}

// listWindowSchedules: GET /v1/windows/{id}/agents/{name}/schedules
func (s *Server) listWindowSchedules(w http.ResponseWriter, r *http.Request) {
	win, name, ok := s.windowSchedulesTarget(w, r)
	if !ok {
		return
	}
	list, err := s.schedules.AgentSchedulesFor(win.ID, name)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"schedules": viewsOf(list)})
}
