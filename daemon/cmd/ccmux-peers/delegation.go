// The delegation tool handlers: delegate (tracked send) and update_task
// (worker status report). Thin proxies onto the daemon's /v1/peers/tasks/*
// endpoints — all state and routing live daemon-side, so a shim restart
// forgets nothing.
package main

import (
	"encoding/json"
	"fmt"
	"strings"
)

func (a *app) toolDelegate(args json.RawMessage) any {
	var in struct {
		ToID           string `json:"to_id"`
		ToName         string `json:"to_name"`
		ToGroup        string `json:"to_group"`
		Message        string `json:"message"`
		SpawnIfMissing bool   `json:"spawn_if_missing"`
	}
	if err := json.Unmarshal(args, &in); err != nil {
		return toolText("Bad arguments: "+err.Error(), true)
	}
	if in.ToID == "" && in.ToName == "" {
		return toolText("Either to_name or to_id is required", true)
	}
	var resp struct {
		OK       bool   `json:"ok"`
		TaskID   string `json:"task_id"`
		Spawning bool   `json:"spawning"`
		Queued   bool   `json:"queued"`
		Error    string `json:"error"`
	}
	if err := a.daemon.post("/v1/peers/tasks/delegate", map[string]any{
		"from_id": a.peerID(), "to_id": in.ToID, "to_name": in.ToName,
		"to_group": in.ToGroup, "text": in.Message, "spawn_if_missing": in.SpawnIfMissing,
	}, &resp); err != nil {
		return toolText("Error delegating: "+err.Error(), true)
	}
	if !resp.OK {
		return toolText("Failed to delegate: "+resp.Error, true)
	}
	who := orID(in.ToName, in.ToID)
	switch {
	case resp.Spawning:
		return toolText(fmt.Sprintf(
			"Delegated as task %s. Worker %q isn't running — ccmux is starting it; the delegation is queued for delivery once it registers. Status updates will arrive as [claude-peers task update] messages.",
			resp.TaskID, in.ToName), false)
	case resp.Queued:
		return toolText(fmt.Sprintf(
			"Delegated as task %s, queued for %s — no session is running there right now; it is delivered the moment one starts. Status updates will arrive as [claude-peers task update] messages.",
			resp.TaskID, who), false)
	default:
		return toolText(fmt.Sprintf(
			"Delegated as task %s to %s. Status updates will arrive as [claude-peers task update] messages; you don't need to ask for status.",
			resp.TaskID, who), false)
	}
}

func (a *app) toolUpdateTask(args json.RawMessage) any {
	var in struct {
		TaskID  string `json:"task_id"`
		Status  string `json:"status"`
		Message string `json:"message"`
		Result  string `json:"result"`
	}
	if err := json.Unmarshal(args, &in); err != nil {
		return toolText("Bad arguments: "+err.Error(), true)
	}
	if in.TaskID == "" || in.Status == "" {
		return toolText("task_id and status are required", true)
	}
	var resp struct {
		OK    bool   `json:"ok"`
		Error string `json:"error"`
	}
	if err := a.daemon.post("/v1/peers/tasks/update", map[string]any{
		"peer_id": a.peerID(), "task_id": in.TaskID, "status": in.Status,
		"message": in.Message, "result": in.Result,
	}, &resp); err != nil {
		return toolText("Error updating task: "+err.Error(), true)
	}
	if !resp.OK {
		return toolText("Failed to update task: "+resp.Error, true)
	}
	return toolText(fmt.Sprintf("Task %s marked %s; the delegator has been notified.", in.TaskID, in.Status), false)
}

// openTask is one row of the daemon's open-delegation listing.
type openTask struct {
	TaskID        string `json:"task_id"`
	FromID        string `json:"from_id"`
	ToID          string `json:"to_id"`
	Status        string `json:"status"`
	StatusMessage string `json:"status_message"`
	Text          string `json:"text"`
}

// openTaskLines renders the peer's open delegations for the check_messages
// tail — the recovery path a delegator uses after its own restart, when the
// task ids are no longer in its context window.
func (a *app) openTaskLines() []string {
	var resp struct {
		Tasks []openTask `json:"tasks"`
	}
	if err := a.daemon.post("/v1/peers/tasks/list", map[string]any{"peer_id": a.peerID()}, &resp); err != nil {
		return nil // enrichment only — a poll must not fail because this did
	}
	var lines []string
	for _, t := range resp.Tasks {
		lines = append(lines, taskLine(t, a.peerID()))
	}
	return lines
}

// taskLine renders one open delegation from selfID's point of view.
func taskLine(t openTask, selfID string) string {
	role, other := "delegated by you to", t.ToID
	if t.ToID == selfID {
		role, other = "yours to do, from", t.FromID
	}
	if other == "" {
		other = "(worker still starting)"
	}
	note := ""
	if t.StatusMessage != "" {
		note = " — " + t.StatusMessage
	}
	return fmt.Sprintf("%s [%s] %s %s%s: %s",
		t.TaskID, t.Status, role, other, note, firstLine(t.Text, 100))
}

// firstLine truncates to the first line and max runes — runes, because
// delegation text is agent prose full of multi-byte punctuation, and a byte
// cut would end previews in a broken rune.
func firstLine(s string, max int) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	if r := []rune(s); len(r) > max {
		return string(r[:max]) + "…"
	}
	return s
}
