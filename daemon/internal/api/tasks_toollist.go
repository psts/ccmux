package api

// What the peers shim's `tasks` tool gets back beside an action's message:
// a list short enough to read. It used to be every task the repo ever had a
// part in, each with its workers' whole reports, and a report is a few
// thousand characters. After 33 tasks on one repo that was 124,000
// characters, more than Claude Code takes from a tool: every worker's
// report, and most of the planning session's filings, came back as an
// error with the real answer saved to a file.

import (
	"fmt"
	"net/http"
	"path/filepath"
	"sort"
	"strings"

	"ccmux.dev/ccmuxd/internal/store"
	"ccmux.dev/ccmuxd/internal/taskq"
)

const (
	// toolFinishedKept is how many finished tasks the list still names: the
	// latest, which a session may want to start a new one after.
	toolFinishedKept = 10
	// toolNoteCut is how much of a note the list shows, in characters.
	toolNoteCut = 200
)

// toolTask is a task as the tool lists it: what the shim prints, no more.
// A part's plan is the bulk of a task and the tool never shows it.
type toolTask struct {
	ID       int64      `json:"id"`
	Title    string     `json:"title"`
	Status   string     `json:"status"`
	AfterIDs []int64    `json:"afterIds,omitempty"`
	Note     string     `json:"note,omitempty"`
	Parts    []toolPart `json:"parts"`
}

type toolPart struct {
	Repo    string `json:"repo"`
	Status  string `json:"status"`
	Note    string `json:"note,omitempty"`
	BusName string `json:"busName"`
	Model   string `json:"model,omitempty"`
	Effort  string `json:"effort,omitempty"`
}

// toolAnswer is the list and how many older finished tasks it leaves out.
type toolAnswer struct {
	Tasks []toolTask
	Older int
}

// toolShows is what one call's list holds: a single task (only), with its
// notes whole or cut, or with only at 0 the repo's open tasks and its
// latest finished ones.
type toolShows struct {
	only  int64
	whole bool
}

// toolShowsFor says what req's answer lists. A builder gets its own task:
// it asked about nothing else. A list that names a task gets that one
// whole, which is how a session reads a worker's full report. Everything
// else gets the short list.
func (s *Server) toolShowsFor(who taskFiler, req paneTaskReq) (toolShows, int, error) {
	switch req.Action {
	case "report", "add_part":
		mine, isWorker, err := s.workerPart(who)
		if err != nil {
			return toolShows{}, http.StatusInternalServerError, err
		}
		if isWorker {
			return toolShows{only: mine.TaskID}, 0, nil
		}
	case "list":
		if req.ID == 0 {
			break
		}
		if _, _, status, err := s.memberTask(who, req.ID); err != nil {
			return toolShows{}, status, err
		}
		return toolShows{only: req.ID, whole: true}, 0, nil
	}
	return toolShows{}, 0, nil
}

// toolAnswerOf builds the list from mine, the tasks with a part in the
// caller's repo in queue order.
func toolAnswerOf(mine []store.Task, parts []store.Part, shows toolShows) toolAnswer {
	listed, older := toolListed(mine, shows.only)
	out := toolAnswer{Tasks: make([]toolTask, 0, len(listed)), Older: older}
	for _, t := range listed {
		v := toolTask{ID: t.ID, Title: t.Title, Status: t.Status, AfterIDs: t.AfterIDs,
			Note: toolNote(t.ID, t.Note, shows.whole), Parts: []toolPart{}}
		for _, p := range taskq.PartsOf(parts, t.ID) {
			v.Parts = append(v.Parts, toolPart{Repo: filepath.Base(p.RepoPath), Status: p.Status,
				Note: toolNote(t.ID, p.Note, shows.whole), BusName: taskq.BusName(p.RepoPath, t.ID),
				Model: p.Model, Effort: p.Effort})
		}
		out.Tasks = append(out.Tasks, v)
	}
	return out
}

// toolListed picks the tasks to list, still in queue order, and counts the
// finished ones left out.
func toolListed(mine []store.Task, only int64) ([]store.Task, int) {
	if only != 0 {
		for _, t := range mine {
			if t.ID == only {
				return []store.Task{t}, 0
			}
		}
		return nil, 0
	}
	keep, older := latestFinished(mine, toolFinishedKept)
	var out []store.Task
	for _, t := range mine {
		if !taskq.Removable(t.Status) || keep[t.ID] {
			out = append(out, t)
		}
	}
	return out, older
}

// latestFinished is the n finished tasks (done or cancelled) changed last,
// by id, and how many more there are.
func latestFinished(mine []store.Task, n int) (map[int64]bool, int) {
	var finished []store.Task
	for _, t := range mine {
		if taskq.Removable(t.Status) {
			finished = append(finished, t)
		}
	}
	sort.Slice(finished, func(i, j int) bool {
		if finished[i].UpdatedAt != finished[j].UpdatedAt {
			return finished[i].UpdatedAt > finished[j].UpdatedAt
		}
		return finished[i].ID > finished[j].ID
	})
	keep := map[int64]bool{}
	for i := 0; i < n && i < len(finished); i++ {
		keep[finished[i].ID] = true
	}
	return keep, len(finished) - len(keep)
}

// toolNote is a note as the list shows it: on one line, and a long one cut
// with a word on where the rest is. whole leaves it as it was written.
func toolNote(taskID int64, note string, whole bool) string {
	if whole {
		return note
	}
	line := []rune(strings.Join(strings.Fields(note), " "))
	if len(line) <= toolNoteCut {
		return string(line)
	}
	return fmt.Sprintf("%s… (cut: list with id %d shows all of it)", string(line[:toolNoteCut]), taskID)
}
