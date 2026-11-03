package main

import (
	"encoding/json"
	"fmt"
	"strings"
)

// queuedTask is the part of a daemon task the tool shows.
type queuedTask struct {
	ID      int64  `json:"id"`
	Title   string `json:"title"`
	Status  string `json:"status"`
	AfterID int64  `json:"afterId"`
	Note    string `json:"note"`
}

// toolTasks forwards the `tasks` tool to the pane's daemon, which takes the
// repo from the pane's own session, and shows this repo's queue after it.
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
	}
	if err := a.daemon.post("/v1/panes/"+a.paneID+"/tasks", in, &resp); err != nil {
		return toolText("Tasks failed: "+err.Error(), true)
	}
	return toolText(taskListText(resp.Message, resp.Tasks), false)
}

func taskListText(message string, tasks []queuedTask) string {
	var b strings.Builder
	if message != "" {
		b.WriteString(message + "\n")
	}
	if len(tasks) == 0 {
		b.WriteString("No tasks for this repo.")
		return b.String()
	}
	b.WriteString("This repo's tasks:\n")
	for _, t := range tasks {
		fmt.Fprintf(&b, "- #%d  %s  [%s]", t.ID, t.Title, t.Status)
		if t.AfterID != 0 {
			fmt.Fprintf(&b, "  after #%d", t.AfterID)
		}
		if t.Note != "" {
			b.WriteString("  " + t.Note)
		}
		b.WriteString("\n")
	}
	return strings.TrimRight(b.String(), "\n")
}
