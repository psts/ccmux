package main

import (
	"encoding/json"
	"fmt"
	"strings"
)

// todoToolText is the todo tool's description. It says when to add, since
// that is the whole point of the tool: nobody asks a session to. The rule
// in the instructions (todoRule) says the same in fewer words, for the
// sessions whose instructions reach them; this is what the others have.
const todoToolText = "A repo's to-do list, kept by ccmux and shown to the human on the thread page. Add what you notice and are NOT handling now, on your own, without being asked: an \"Also found\" item, a review finding you leave, a follow-up, a bug you saw on the way. One line each, with where it is. Urgency: high if it breaks something or loses data, medium if it will bite soon, low otherwise. Actions: add (text, urgency; repo for another repo's list, default your own), list (the repo's unfinished items; look before you add), done (id, note: you fixed an item on the list). The same item added twice is kept once."

var todoTool = map[string]any{
	"name":        "todo",
	"description": todoToolText,
	"inputSchema": map[string]any{
		"type": "object",
		"properties": map[string]any{
			"action": map[string]any{
				"type":        "string",
				"enum":        []string{"add", "list", "done"},
				"description": "What to do.",
			},
			"text": map[string]any{
				"type":        "string",
				"description": "add: the item, one line: what is wrong or left to do, and where (file, function).",
			},
			"urgency": map[string]any{
				"type":        "string",
				"enum":        []string{"high", "medium", "low"},
				"description": "add: high if it breaks something or loses data, medium if it will bite soon, low otherwise.",
			},
			"repo": map[string]any{
				"type":        "string",
				"description": "Another repo's list: the folder name of a repo in this session's ccmux window. Default: your own repo.",
			},
			"id": map[string]any{
				"type":        "integer",
				"description": "done: the item's number.",
			},
			"note": map[string]any{
				"type":        "string",
				"description": "done: how it was fixed, in a few words (a commit, a file).",
			},
		},
		"required": []string{"action"},
	},
}

// listedTodo is the part of an item the tool shows.
type listedTodo struct {
	ID      int64  `json:"id"`
	Text    string `json:"text"`
	Urgency string `json:"urgency"`
	Status  string `json:"status"`
	TaskID  int64  `json:"taskId"`
	AddedBy string `json:"addedBy"`
}

// toolTodo forwards the `todo` tool to the pane's daemon, which takes the
// repo from the pane's own session, and adds this session's bus name: who
// added an item, or finished it.
func (a *app) toolTodo(args json.RawMessage) any {
	if a.paneID == "" {
		return toolText("The to-do lists need a ccmux session; this one has no pane.", true)
	}
	var in map[string]any
	if err := json.Unmarshal(args, &in); err != nil {
		return toolText("Bad arguments: "+err.Error(), true)
	}
	if in == nil {
		in = map[string]any{}
	}
	in["by"] = a.name
	var resp struct {
		Message string       `json:"message"`
		Repo    string       `json:"repo"`
		Todos   []listedTodo `json:"todos"`
	}
	if err := a.daemon.post("/v1/panes/"+a.paneID+"/todo", in, &resp); err != nil {
		return toolText("To-do failed: "+err.Error(), true)
	}
	if in["action"] == "list" {
		return toolText(todoListText(resp.Repo, resp.Todos), false)
	}
	return toolText(resp.Message, false)
}

// todoListText is the answer to a list: the repo's unfinished items, in
// the order the daemon reads them (high first, newest first).
func todoListText(repo string, list []listedTodo) string {
	if len(list) == 0 {
		return fmt.Sprintf("Nothing unfinished on %s's to-do list.", repo)
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%s's to-do list, %d unfinished:\n", repo, len(list))
	for _, t := range list {
		b.WriteString(todoLine(t) + "\n")
	}
	return strings.TrimRight(b.String(), "\n")
}

// todoLine is one item: its number, urgency and text, then that a task has
// it (so nobody starts on it by hand) and who added it.
func todoLine(t listedTodo) string {
	line := fmt.Sprintf("- #%d [%s] %s", t.ID, t.Urgency, t.Text)
	switch {
	case t.TaskID != 0:
		line += fmt.Sprintf("  (task #%d has it)", t.TaskID)
	case t.Status == "tasked":
		line += "  (being filed as a task)"
	}
	if t.AddedBy != "" {
		line += "  added by " + t.AddedBy
	}
	return line
}
