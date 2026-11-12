package main

import (
	"encoding/json"
	"fmt"
	"strings"
)

// queuedTask is the part of a daemon task the tool shows.
type queuedTask struct {
	ID       int64        `json:"id"`
	Title    string       `json:"title"`
	Status   string       `json:"status"`
	AfterIDs []int64      `json:"afterIds"`
	Look     bool         `json:"look"`
	Note     string       `json:"note"`
	Parts    []queuedPart `json:"parts"`
}

// queuedPart is one repo's part of a task, as the tool shows it. Model and
// Effort are what its builder starts with, empty on a part from before
// parts had them.
type queuedPart struct {
	Repo    string `json:"repo"`
	Status  string `json:"status"`
	Note    string `json:"note"`
	BusName string `json:"busName"`
	Model   string `json:"model"`
	Effort  string `json:"effort"`
}

// toolTasks forwards the `tasks` tool to the pane's daemon, which takes the
// repo from the pane's own session, and shows this repo's tasks after it.
func (a *app) toolTasks(args json.RawMessage) any {
	if a.paneID == "" {
		return toolText("The task queue needs a ccmux session; this one has no pane.", true)
	}
	var in map[string]any
	if err := json.Unmarshal(args, &in); err != nil {
		return toolText("Bad arguments: "+err.Error(), true)
	}
	var resp struct {
		Message string       `json:"message"`
		Tasks   []queuedTask `json:"tasks"`
		Older   int          `json:"older"`
	}
	if err := a.daemon.post("/v1/panes/"+a.paneID+"/tasks", in, &resp); err != nil {
		return toolText("Tasks failed: "+err.Error(), true)
	}
	return toolText(taskListText(resp.Message, resp.Tasks, resp.Older), false)
}

// taskListText is the tool's answer: the action's message, then the tasks
// the daemon chose to list (its short list: open tasks and the latest
// finished, notes cut), then how many older finished ones it left out.
func taskListText(message string, tasks []queuedTask, older int) string {
	var b strings.Builder
	if message != "" {
		b.WriteString(message + "\n")
	}
	if len(tasks) == 0 {
		b.WriteString("No tasks with a part in this repo.")
		return b.String()
	}
	b.WriteString("Tasks with a part in this repo:\n")
	for _, t := range tasks {
		fmt.Fprintf(&b, "- #%d  %s  [%s]", t.ID, t.Title, t.Status)
		if len(t.AfterIDs) > 0 {
			b.WriteString("  after " + taskNumbers(t.AfterIDs))
		}
		if t.Look {
			b.WriteString("  (a person reads it before it ships)")
		}
		b.WriteString("\n")
		for _, p := range t.Parts {
			b.WriteString(partLine(p) + "\n")
		}
	}
	if older > 0 {
		fmt.Fprintf(&b, "(%d older finished tasks are not listed; list with an id shows one whole.)", older)
	}
	return strings.TrimRight(b.String(), "\n")
}

// taskNumbers names tasks in a row: "#58, #71".
func taskNumbers(ids []int64) string {
	names := make([]string, len(ids))
	for i, id := range ids {
		names[i] = fmt.Sprintf("#%d", id)
	}
	return strings.Join(names, ", ")
}

// partLine is one part in the list: its repo, status and builder's bus
// name, the model and effort it is built with (so a session sees that a
// part it filed as hard is one), and its note.
func partLine(p queuedPart) string {
	line := fmt.Sprintf("    %s: %s (%s)", p.Repo, p.Status, p.BusName)
	if p.Model != "" {
		line += fmt.Sprintf("  built with %s, %s effort", p.Model, p.Effort)
	}
	if p.Note != "" {
		line += "  " + p.Note
	}
	return line
}
