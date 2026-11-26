package api

// The planners' minute pass: a planner whose line is empty ends once it has
// gone cold, and a line whose planner ended gets a new one.

import (
	"log"
	"path/filepath"

	"ccmux.dev/ccmuxd/internal/model"
	"ccmux.dev/ccmuxd/internal/store"
)

// PlannerPass brings the planners in line with the planning lines. Called
// on a timer (cmd/ccmuxd), under the lock Make task takes, so a line is
// never read as empty while an item is on its way into it.
//
//   - A planner whose repo's line is empty is ended once it is cold (its
//     last turn ended longer ago than its prompt cache lives, no card, no
//     message waiting) and owes nobody a delegation. Its side helpers end
//     with it (threads.SessionEnded). A planner with items still in its
//     line is never ended here, however long it has been quiet: the item's
//     row links to its conversation, and a person sends it again or puts it
//     back.
//   - A line with no planner (it crashed for good, or a person ended it)
//     gets a new one, a fresh conversation, which is sent the whole line.
func (s *Server) PlannerPass() {
	if s.todos == nil || s.runners == nil || s.runnerRows == nil {
		return
	}
	s.plannerMu.Lock()
	defer s.plannerMu.Unlock()
	lines, err := s.planningLines()
	if err != nil {
		log.Printf("todos: the planners were not checked, the planning lines were not read: %v", err)
		return
	}
	planners, err := s.runnerRows.RunnerPlanners()
	if err != nil {
		log.Printf("todos: the planners were not checked, their rows were not read: %v", err)
		return
	}
	repos := s.todoRepos()
	has := map[string]bool{}
	for _, p := range planners {
		key := repos[p.WorkspaceID]
		has[key] = true
		if len(lines[key]) == 0 {
			s.endIdlePlanner(p)
		}
	}
	for key, line := range lines {
		if !has[key] {
			s.replacePlanner(key, line)
		}
	}
}

// planningLines is every repo's planning line, by the list's key.
func (s *Server) planningLines() (map[string][]store.Todo, error) {
	all, err := s.todos.Line("")
	if err != nil {
		return nil, err
	}
	out := map[string][]store.Todo{}
	for _, t := range all {
		out[t.Repo] = append(out[t.Repo], t)
	}
	return out, nil
}

// endIdlePlanner ends a planner with nothing left in its line, once nothing
// would be lost by it.
func (s *Server) endIdlePlanner(p store.RunnerSession) {
	d := s.plannerDoors
	if d.Cold == nil || (d.Owes != nil && p.PaneID != "" && d.Owes(p.PaneID)) || !d.Cold(p.ID) {
		return
	}
	if err := s.runners.SetWanted(p.ID, store.SessionDone); err != nil {
		log.Printf("todos: planner %s is idle with an empty line and was not ended: %v", p.ID, err)
		return
	}
	log.Printf("todos: planner %s ended: its line is empty and its cache had gone cold", p.ID)
}

// replacePlanner starts a planner for a line that has none and sends it the
// whole line, on the model the repo's last planner ran on.
func (s *Server) replacePlanner(key string, line []store.Todo) {
	name := filepath.Base(key)
	ws := s.plannerHome(key)
	if ws == nil {
		s.plannerSays(key, "no session of the repo is open in a ccmux window, so its items wait in planning")
		return
	}
	modelName, err := s.runnerRows.LastPlannerModel(ws.ID)
	if err != nil {
		log.Printf("todos: %s's last planner's model was not read, the new one starts on the default: %v", name, err)
	}
	planner, made, err := s.ensurePlanner(ws, key, modelName)
	if err != nil {
		s.plannerSays(key, "a new planner could not be started: "+err.Error())
		return
	}
	if _, err := s.runners.Send(planner.ID, plannerPrompt(name, line, nil), plannerSender); err != nil {
		s.dropUnfed(planner, made)
		s.plannerSays(key, "a new planner could not be sent the line: "+err.Error())
		return
	}
	s.plannerSays(key, "")
	log.Printf("todos: %s's planning line had no planner: started %s with its %d items", name, planner.ID, len(line))
}

// plannerHome is the repo session a line's planner is made in: the first of
// the repo's sessions that is in a ccmux window, whose queue its tasks go
// into. nil when the repo has none.
func (s *Server) plannerHome(key string) *model.Workspace {
	repos := s.todoRepos()
	for _, ws := range s.mgr.List() {
		if repos[ws.ID] != key {
			continue
		}
		if window, err := s.mgr.WindowOfWorkspace(ws.ID); err == nil && window != "" {
			return ws
		}
	}
	return nil
}

// plannerSays logs why a line has no planner, once for as long as the
// reason stays the same: the pass runs every minute. "" forgets it.
func (s *Server) plannerSays(key, why string) {
	if s.plannerSaid == nil {
		s.plannerSaid = map[string]string{}
	}
	if s.plannerSaid[key] == why {
		return
	}
	s.plannerSaid[key] = why
	if why != "" {
		log.Printf("todos: %s's planning line has no planner: %s", filepath.Base(key), why)
	}
}
