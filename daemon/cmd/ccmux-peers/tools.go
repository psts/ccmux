// The MCP tools. The original four keep schemas and wording verbatim from the
// old claude-peers server.ts — running sessions are trained on these exact
// texts; list_peers additively gained an "all" scope. delegate and update_task
// (handlers in delegation.go) are the tracked-delegation surface added
// 2026-08: same addressing as send_message plus a durable task row.
package main

import (
	"encoding/json"
	"fmt"
	"strings"
)

// serverInstructions goes into the session's system prompt. Most of it is
// frozen wording that running sessions pattern-match on, but the SendMessage
// paragraph deliberately is not: it used to say SendMessage "is for subagent
// communication, not peer messaging", and Claude Code v2.1.224 made that false
// by giving SendMessage cross-session delivery. The directive is unchanged and
// still right — one bus, this one — but a true directive resting on a false
// premise is worse than no reason at all, because the session can check the
// premise and will then weigh the whole block accordingly.
const serverInstructions = `You are connected to the claude-peers messaging network. Peers are named after their repo directory (e.g., "backend", "website").

Messages arrive as <channel source="claude-peers" from_name="..." from_id="..." from_summary="..." from_cwd="...">. When you receive one, RESPOND IMMEDIATELY — pause your current work, reply, then resume. Treat it like a coworker tapping your shoulder.

To reply, call the send_message tool (snake_case) with to_id set to the from_id from the channel tag. Always use to_id (not to_name) for replies — multiple peers can share a repo name, and to_id guarantees the reply reaches the specific peer that messaged you. Do NOT use the built-in SendMessage tool, and discover peers with list_peers rather than ListAgents. SendMessage can reach other Claude Code sessions, so this is not a claim that it would fail — it is that it goes around this bus: nothing is queued for a peer whose session is not running, nothing shows up in ccmux for the human, and delegation tracking and the permission relay do not see it. The correct tool for peer messaging is always send_message.

When a peer asks you to do work, acknowledge the request, do the work, and then send a completion message back to that peer summarizing what you did and the outcome (success, failure, or questions). Always close the loop — the peer that gave you the task should not have to ask for a status update.

PERMISSION RELAY: If you receive a message starting with "[claude-peers permission relay]", another peer needs your approval to run a tool. The relay is broadcast to everyone who has messaged that peer recently — so it might be for work YOU delegated, or it might be for work someone else delegated. ONLY respond yes/no if you actually asked that peer to do the work in question. If you didn't delegate it, ignore the relay completely (don't send anything back). To approve work you delegated: call send_message back to that peer with the message field set to exactly "yes <request_id>" (e.g. "yes abcde"). To deny: "no <request_id>". The reply must contain only those two tokens — no greeting, no explanation. The request_id is the five lowercase letters in the relay message.

DELEGATION TASKS: If a message starts with "[claude-peers delegation task tsk_xxxxxxxx]", you are the worker on a tracked delegation. Call update_task(task_id, status="acked") immediately, "working" when you start, and close it with status="completed" plus a result summary (or "failed" and why). Your updates reach the delegator automatically — do not also send a separate completion message. When YOU need a peer to do work whose completion you must know about, use delegate instead of send_message; you will receive "[claude-peers task update]" messages as the worker reports, and check_messages lists your open delegations. Task update messages are status notifications, not conversation: read them, act if the result requires it, and do NOT send a reply unless something is wrong. A delegator may close its own task with update_task(status="failed") to cancel it.

Available tools:
- send_message: Reply to a peer by passing to_id (from the tag's from_id) and message. For unsolicited outbound messages, pass to_name instead.
- delegate: Send tracked work to a peer; returns a task_id whose status updates arrive automatically.
- update_task: Report status on a delegation you received (acked/working/completed/failed).
- list_peers: Discover other Claude Code instances in your project.
- set_summary: Describe what you're working on (visible to peers).
- check_messages: Manually poll for messages (usually automatic via channel push); also lists open delegations.

On startup, call set_summary to describe your current work.`

// toolsList is the tools/list response payload.
var toolsList = []map[string]any{
	{
		"name":        "list_peers",
		"description": "List other Claude Code instances in your project. Returns their name, ID, working directory, and summary.",
		"inputSchema": map[string]any{
			"type": "object",
			"properties": map[string]any{
				"scope": map[string]any{
					"type":        "string",
					"enum":        []string{"project", "directory", "repo", "all"},
					"description": `Scope of peer discovery. "project" (default) = all instances in your window group (sibling repos for sessions outside ccmux). "directory" = same working directory. "repo" = same git repository. "all" = every peer on this machine.`,
					"default":     "project",
				},
				"group": map[string]any{
					"type":        "string",
					"description": `Look into ANOTHER project instead of your own — pass the group name exactly as shown in a peer's "Group:" line. Use this when you've been asked about someone in a different project ("who's running in ChartLabs?"). Omit for your own group, which is the normal case.`,
				},
			},
		},
	},
	{
		"name":        "send_message",
		"description": `Send a message to another Claude Code peer. To reply to a <channel source="claude-peers"> message, pass to_id from the tag's from_id attribute — this guarantees the reply reaches the specific peer that messaged you, even if multiple peers share a repo name. For an unsolicited outbound message, pass to_name. Use this tool, not the built-in SendMessage.`,
		"inputSchema": map[string]any{
			"type": "object",
			"properties": map[string]any{
				"to_name": map[string]any{
					"type":        "string",
					"description": `Peer name to message — use this for unsolicited outbound messages (e.g., "backend", "website"). For replies, prefer to_id to disambiguate when multiple peers share a repo name.`,
				},
				"to_id": map[string]any{
					"type":        "string",
					"description": "The peer ID of the target. Use this when replying to a received <channel> message — read it from the tag's from_id attribute. Guarantees the reply reaches the specific peer that messaged you.",
				},
				"message": map[string]any{
					"type":        "string",
					"description": "The message to send",
				},
				"to_group": map[string]any{
					"type":        "string",
					"description": `Message a peer in ANOTHER project. By default you can only reach your own group; naming the target's group here authorizes the crossing. Use it when you've been told to contact someone in a specific project — e.g. send_message(to_name="backend", to_group="ChartLabs", message="..."). Find group names with list_peers. Not needed to REPLY to someone who messaged you from another project.`,
				},
				"spawn_if_missing": map[string]any{
					"type":        "boolean",
					"description": `If the named teammate (to_name) isn't running, ask the session manager (ccmux) to start a fresh Claude Code instance in that repo and queue this message for delivery once it comes up. Only works with to_name. Use this to reach a teammate that hasn't been started yet — e.g. send_message(to_name="backend", spawn_if_missing=true, message="..."). The teammate replies over the channel when ready, or you get an "unreachable" notice if it can't be started.`,
				},
			},
			"required": []string{"message"},
		},
	},
	{
		"name":        "delegate",
		"description": `Send TRACKED work to another peer. Same addressing as send_message, but the bus creates a durable task: the worker reports progress with update_task and you receive "[claude-peers task update]" messages automatically — no need to ask for status or chase silence. Use this instead of send_message whenever you need to know the outcome of the work. Returns the task_id.`,
		"inputSchema": map[string]any{
			"type": "object",
			"properties": map[string]any{
				"to_name": map[string]any{
					"type":        "string",
					"description": `Worker peer name (e.g., "backend"). Prefer to_id when replying threads exist or names are ambiguous.`,
				},
				"to_id": map[string]any{
					"type":        "string",
					"description": "Worker peer ID (from a channel tag's from_id or list_peers).",
				},
				"message": map[string]any{
					"type":        "string",
					"description": "The work being delegated. Self-contained: contract, constraints, and what a completed result must include.",
				},
				"to_group": map[string]any{
					"type":        "string",
					"description": "Delegate into ANOTHER project; naming the target's group authorizes the crossing (same rule as send_message).",
				},
				"spawn_if_missing": map[string]any{
					"type":        "boolean",
					"description": "If the named worker (to_name) isn't running, ask ccmux to start it and queue the delegation for delivery once it registers.",
				},
			},
			"required": []string{"message"},
		},
	},
	{
		"name":        "update_task",
		"description": `Report status on a delegation you received (its message opened with "[claude-peers delegation task tsk_...]"). Call with status="acked" on receipt, "working" when you start, and close with "completed" plus result (or "failed" and why). Each update is delivered to the delegator automatically.`,
		"inputSchema": map[string]any{
			"type": "object",
			"properties": map[string]any{
				"task_id": map[string]any{
					"type":        "string",
					"description": `The task id from the delegation header, e.g. "tsk_ab12cd34".`,
				},
				"status": map[string]any{
					"type":        "string",
					"enum":        []string{"acked", "working", "completed", "failed"},
					"description": "The transition to report.",
				},
				"message": map[string]any{
					"type":        "string",
					"description": "One-line progress note shown to the delegator with the status.",
				},
				"result": map[string]any{
					"type":        "string",
					"description": `The outcome, for "completed" (what was done, where, verification) or "failed" (what blocked it).`,
				},
			},
			"required": []string{"task_id", "status"},
		},
	},
	{
		"name":        "set_summary",
		"description": "Set a brief summary (1-2 sentences) of what you are currently working on. This is visible to other Claude Code instances when they list peers.",
		"inputSchema": map[string]any{
			"type": "object",
			"properties": map[string]any{
				"summary": map[string]any{
					"type":        "string",
					"description": "A 1-2 sentence summary of your current work",
				},
			},
			"required": []string{"summary"},
		},
	},
	{
		"name":        "check_messages",
		"description": "Manually check for new messages from other Claude Code instances. Messages are normally pushed automatically via channel notifications, but you can use this as a fallback.",
		"inputSchema": map[string]any{
			"type":       "object",
			"properties": map[string]any{},
		},
	},
}

// Claude Code defers MCP tools behind its ToolSearch step unless they opt out,
// and a deferred tool is not in context when the session reads its first
// instruction. Every tool here opts out, because every one of them is named by
// text the session is guaranteed to meet and told to act on at once:
// serverInstructions says to answer a channel message IMMEDIATELY with
// send_message and to call set_summary on startup, a delegation header says to
// call update_task now, the relay text says to answer with send_message, and a
// poll-only session has no way to collect its mail except check_messages. A
// tool that a standing order names but that is not loaded is a trap — the
// session reads the order and cannot obey it without a search round trip it was
// never told to make.
//
// The cost is real and is the reason this is a deliberate list rather than a
// default: these six schemas sit in every session's context from startup. Drop
// a tool from the loop below if that trade stops being worth it, but drop it
// from the instructions in the same edit.
func init() {
	for _, tool := range toolsList {
		tool["_meta"] = map[string]any{"anthropic/alwaysLoad": true}
	}
}

func (a *app) callTool(name string, args json.RawMessage) any {
	if a.peerID() == "" {
		return toolText("Not registered with broker yet", true)
	}
	switch name {
	case "list_peers":
		return a.toolListPeers(args)
	case "send_message":
		return a.toolSendMessage(args)
	case "delegate":
		return a.toolDelegate(args)
	case "update_task":
		return a.toolUpdateTask(args)
	case "set_summary":
		return a.toolSetSummary(args)
	case "check_messages":
		return a.toolCheckMessages()
	default:
		return toolText("Unknown tool: "+name, true)
	}
}

type listEntry struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Group     string `json:"group"`
	CWD       string `json:"cwd"`
	GitRoot   string `json:"git_root"`
	Summary   string `json:"summary"`
	Connected bool   `json:"connected"`
	PollOnly  bool   `json:"poll_only"`
}

func (a *app) toolListPeers(args json.RawMessage) any {
	var in struct {
		Scope string `json:"scope"`
		Group string `json:"group"`
	}
	_ = json.Unmarshal(args, &in)
	if in.Scope == "" {
		in.Scope = "project"
	}
	var peers []listEntry
	if err := a.daemon.post("/v1/peers/list", map[string]any{
		"peer_id": a.peerID(), "scope": in.Scope, "group": in.Group,
	}, &peers); err != nil {
		return toolText("Error listing peers: "+err.Error(), true)
	}
	if len(peers) == 0 {
		return toolText(fmt.Sprintf("No other Claude Code instances found (scope: %s, project: %s).",
			in.Scope, a.group()), false)
	}
	lines := make([]string, 0, len(peers))
	for _, p := range peers {
		parts := []string{
			"Name: " + orID(p.Name, "(unnamed)"),
			"ID: " + p.ID,
			"Group: " + p.Group, // pass this as to_group to message across projects
			"CWD: " + p.CWD,
		}
		if p.GitRoot != "" {
			parts = append(parts, "Repo: "+p.GitRoot)
		}
		if p.Summary != "" {
			parts = append(parts, "Summary: "+p.Summary)
		}
		parts = append(parts, "Status: "+peerStatus(p))
		lines = append(lines, strings.Join(parts, "\n  "))
	}
	return toolText(fmt.Sprintf("Found %d peer(s) (scope: %s, project: %s):\n\n%s",
		len(peers), in.Scope, a.group(), strings.Join(lines, "\n\n")), false)
}

func (a *app) toolSendMessage(args json.RawMessage) any {
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
		Spawning bool   `json:"spawning"`
		Queued   bool   `json:"queued"`
		Error    string `json:"error"`
	}
	if err := a.daemon.post("/v1/peers/send", map[string]any{
		"from_id": a.peerID(), "to_id": in.ToID, "to_name": in.ToName,
		"to_group": in.ToGroup,
		"text":     in.Message, "spawn_if_missing": in.SpawnIfMissing,
	}, &resp); err != nil {
		return toolText("Error sending message: "+err.Error(), true)
	}
	if !resp.OK {
		return toolText("Failed to send: "+resp.Error, true)
	}
	if resp.Spawning {
		return toolText(fmt.Sprintf(`Teammate "%s" isn't running — asked ccmux to start it. Your message is queued and will be delivered once it registers; it'll reply over the channel when ready (or you'll get an "unreachable" notice if it can't be started).`, in.ToName), false)
	}
	if resp.Queued {
		return toolText(fmt.Sprintf(
			"Queued for %s — no Claude session is running there right now. It stays in that pane's inbox and is delivered the moment a session starts there; nobody will read it before then. Don't wait on a reply.",
			orID(in.ToName, in.ToID)), false)
	}
	if in.ToName != "" {
		return toolText(fmt.Sprintf("Message sent to %q", in.ToName), false)
	}
	return toolText("Message sent to peer "+in.ToID, false)
}

func (a *app) toolSetSummary(args json.RawMessage) any {
	var in struct {
		Summary string `json:"summary"`
	}
	if err := json.Unmarshal(args, &in); err != nil {
		return toolText("Bad arguments: "+err.Error(), true)
	}
	if err := a.daemon.post("/v1/peers/summary", map[string]any{
		"peer_id": a.peerID(), "summary": in.Summary,
	}, nil); err != nil {
		return toolText("Error setting summary: "+err.Error(), true)
	}
	return toolText(fmt.Sprintf("Summary updated: %q", in.Summary), false)
}

func (a *app) toolCheckMessages() any {
	var resp struct {
		Events []wireEvent `json:"events"`
	}
	// Read BEFORE the poll: these seqs belong to whichever bus answers it, and a
	// move landing before we mark them would otherwise stamp this bus's numbering
	// onto the next one (see markShown).
	epoch := a.busEpochNow()
	if err := a.daemon.post("/v1/peers/poll", map[string]any{"peer_id": a.peerID()}, &resp); err != nil {
		return toolText("Error checking messages: "+err.Error(), true)
	}
	var lines []string
	for _, ev := range resp.Events {
		// The daemon's cursor only advances when this process acks a push, and it
		// acks only after the notification write succeeds. So a poll racing a
		// still-unacked push legitimately gets that event back — and rendering it
		// would show the session the same message twice.
		if a.alreadyShown(ev.Seq) {
			continue
		}
		if ev.Type == "permission_verdict" {
			// A verdict that arrived while the push channel was down still
			// resolves the dialog — emit it, don't render it as chat.
			if a.mcp.Notify("notifications/claude/channel/permission", map[string]any{
				"request_id": ev.RequestID, "behavior": ev.Behavior,
			}) == nil {
				a.markShown(ev.Seq, epoch)
			}
			continue
		}
		a.markShown(ev.Seq, epoch)
		lines = append(lines, fmt.Sprintf("From %s (%s):\n%s", orID(ev.FromName, ev.FromID), ev.SentAt, ev.Text))
	}
	body := "No new messages."
	if len(lines) > 0 {
		body = fmt.Sprintf("%d new message(s):\n\n%s", len(lines), strings.Join(lines, "\n\n---\n\n"))
	}
	// Open delegations ride along so a restarted session can pick its threads
	// back up without the task ids having survived in its context window.
	if tasks := a.openTaskLines(); len(tasks) > 0 {
		body += "\n\nOpen delegation task(s):\n" + strings.Join(tasks, "\n")
	}
	return toolText(body, false)
}

// peerStatus describes a listed peer. Every peer in a listing has a live Claude
// session behind it, so the question is only how fast it will hear you: a poll
// -only session holds no socket by design and must not be reported as broken,
// which is what the old blanket "(reconnecting)" did to it forever.
func peerStatus(p listEntry) string {
	switch {
	case p.PollOnly:
		return "online (polls for messages — delivery on its next check)"
	case p.Connected:
		return "online"
	default:
		return "online (reconnecting)"
	}
}
