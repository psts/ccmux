package api

// The repo planners (store/planners.go). "Make task" on a repo's to-do list
// puts the item in the repo's planning line (todos.Book.Plan) and sends it
// to the repo's planner: one session per repo, of kind planner, made with
// the first item and ended once its line is empty and its turn is over
// (PlannerPass). The planner plans each item and files it with the tasks
// tool, which takes the item out of the line. The repo's main session is
// never prompted: its conversation is its person's.

import (
	"errors"
	"fmt"
	"log"
	"net/http"
	"path/filepath"
	"strings"

	"ccmux.dev/ccmuxd/internal/model"
	"ccmux.dev/ccmuxd/internal/runner"
	"ccmux.dev/ccmuxd/internal/store"
	"ccmux.dev/ccmuxd/internal/todos"
)

// plannerTitle is what a planner's row and header show. It is set when the
// session is made, so its first prompt (an item of the line) never names it.
const plannerTitle = "Planner"

// plannerSender is who the daemon's own messages to a planner are from.
const plannerSender = "ccmux"

// PlannerDoors is what ending an idle planner asks of the rest of the
// daemon: Finished says a session's turn has ended and nothing waits on it
// (runner.Supervisor.Finished), Owes that its peer still owes a delegator
// a result. Without Finished no planner is ended.
type PlannerDoors struct {
	Finished func(sessionID string) bool
	Owes     func(paneID string) bool
}

// SetPlannerDoors wires what PlannerPass ends an idle planner on.
func (s *Server) SetPlannerDoors(d PlannerDoors) { s.plannerDoors = d }

// plannerOf is the planner of the repo whose list is key, in whichever of
// the repo's sessions it was made; nil when the repo has none that has not
// ended.
func (s *Server) plannerOf(key string) (*store.RunnerSession, error) {
	list, err := s.runnerRows.RunnerPlanners()
	if err != nil {
		return nil, err
	}
	repos := s.todoRepos()
	for i := range list {
		if repos[list[i].WorkspaceID] == key {
			return &list[i], nil
		}
	}
	return nil, nil
}

// ensurePlanner is the planner of the repo whose list is key, made in repo
// session ws (on modelName, "" for the default) when the repo has none.
// made says it is new: nothing was ever sent to it.
func (s *Server) ensurePlanner(ws *model.Workspace, key, modelName string) (store.RunnerSession, bool, error) {
	found, err := s.plannerOf(key)
	if err != nil {
		return store.RunnerSession{}, false, err
	}
	if found != nil {
		return *found, false, nil
	}
	sess, err := s.runners.Create(runner.NewSession{Cwd: ws.RepoPath, Model: modelName, WorkspaceID: ws.ID, Kind: store.SessionPlanner, Title: plannerTitle})
	if errors.Is(err, store.ErrPlannerExists) {
		if found, rerr := s.plannerOf(key); rerr == nil && found != nil {
			return *found, false, nil // made between the look and the insert
		}
	}
	return sess, err == nil, err
}

// handToPlanner puts item id in its repo's planning line and sends it to
// the repo's planner, made in ws when the repo has none. An item that
// reached no planner is put back as it was: it is never left marked as
// handed on when nothing was sent. The caller holds plannerMu.
func (s *Server) handToPlanner(id int64, ws *model.Workspace, modelName, sender string) (store.Todo, string, int, error) {
	item, was, err := s.todos.Plan(id)
	if err != nil {
		return store.Todo{}, "", todoStatus(err), err
	}
	planner, status, err := s.feedPlanner(ws, item, modelName, sender)
	if err != nil {
		if uerr := s.todos.Unplan(was); uerr != nil {
			log.Printf("todos: #%d reached no planner and was not put back as it was: %v", id, uerr)
		}
		return store.Todo{}, "", status, err
	}
	return item, planner.ID, 0, nil
}

// feedPlanner sends item, which is in its repo's line, to the repo's
// planner. A planner that is new gets the whole line: what an earlier
// planner left behind was never sent to this one.
func (s *Server) feedPlanner(ws *model.Workspace, item store.Todo, modelName, sender string) (store.RunnerSession, int, error) {
	planner, made, err := s.ensurePlanner(ws, item.Repo, modelName)
	if err != nil {
		return planner, http.StatusInternalServerError, fmt.Errorf("starting %s's planner: %w", ws.Name, err)
	}
	line, err := s.todos.Line(item.Repo)
	if err != nil {
		s.dropUnfed(planner, made)
		return planner, http.StatusInternalServerError, err
	}
	text := plannerPrompt(filepath.Base(item.Repo), []store.Todo{item}, without(line, item.ID))
	if made {
		text = plannerPrompt(filepath.Base(item.Repo), line, nil)
	}
	if _, err := s.runners.Send(planner.ID, text, sender); err != nil {
		s.dropUnfed(planner, made)
		return planner, runnerStatus(err), err
	}
	return planner, 0, nil
}

// dropUnfed ends a planner that was just made and could not be sent its
// line: it would never finish a turn, so it would never end.
func (s *Server) dropUnfed(planner store.RunnerSession, made bool) {
	if !made {
		return
	}
	if err := s.runners.SetWanted(planner.ID, store.SessionDone); err != nil {
		log.Printf("todos: planner %s was sent nothing and was not ended: %v", planner.ID, err)
	}
}

// without is list with item id left out.
func without(list []store.Todo, id int64) []store.Todo {
	out := []store.Todo{}
	for _, t := range list {
		if t.ID != id {
			out = append(out, t)
		}
	}
	return out
}

// plannerPrompt is what a planner is sent: the items to plan now, and the
// others in its line, so it sees what belongs together. Each message names
// the tool's argument (todo), since that is what takes an item out of the
// line: without it the item would stay in planning for good.
func plannerPrompt(repo string, items, others []store.Todo) string {
	var b strings.Builder
	if len(items) == 1 {
		fmt.Fprintf(&b, "To-do #%d is in %s's planning line. Plan it and file it as a task: tasks tool, action file, with todo: %d.\n", items[0].ID, repo, items[0].ID)
	} else {
		fmt.Fprintf(&b, "%d to-do items are in %s's planning line, the longest in it first. Plan each and file it as a task: tasks tool, action file, with todo: the item's number "+
			"(a list of numbers when one task covers several items).\n", len(items), repo)
	}
	for _, t := range items {
		b.WriteString(lineItem(t))
	}
	if len(others) > 0 {
		b.WriteString("\nAlso in the line, each sent to you in a message of its own (merge or order them with this one when that is better):\n")
		for _, t := range others {
			b.WriteString(lineItem(t))
		}
	}
	return strings.TrimRight(b.String(), "\n")
}

func lineItem(t store.Todo) string {
	return fmt.Sprintf("- #%d [%s] %s\n", t.ID, t.Urgency, t.Text)
}

// leftLine tells a repo's planner that a person took an item out of its
// line (how: put it back on the list, marked it done, deleted it), so it
// does not file a task for it after all. before is the item as it was; one
// that was not in the line is nobody's to tell. Best effort: a planner that
// was not told files a task a person can cancel.
func (s *Server) leftLine(before store.Todo, how string) {
	if before.Status != todos.Planning || s.runners == nil || s.runnerRows == nil {
		return
	}
	planner, err := s.plannerOf(before.Repo)
	if err != nil || planner == nil {
		return
	}
	text := fmt.Sprintf("A person took to-do #%d out of your planning line (they %s): do not plan or file it. "+
		"If you filed a task for it already, leave the task and say so in one line. (#%d: %s)", before.ID, how, before.ID, before.Text)
	if _, err := s.runners.Send(planner.ID, text, plannerSender); err != nil {
		log.Printf("todos: planner %s was not told that #%d left its line: %v", planner.ID, before.ID, err)
	}
}

// howItLeft is the words for the status a person moved a planning item to.
func howItLeft(status string) string {
	if status == todos.Done {
		return "marked it done"
	}
	return "put it back on the list"
}
