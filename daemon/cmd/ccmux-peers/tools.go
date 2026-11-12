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

// serverInstructions goes into the session's system prompt, ahead of the
// per-session parts (instructions()). Claude Code keeps only the first 4096
// characters of a server's instructions, so this is kept short: every relay
// and delegation format a session must match stays word for word, the prose
// around them is tight, and the tool list is gone (each tool's own
// description already says it). The SendMessage reason stays true: Claude
// Code v2.1.224 gave SendMessage cross-session delivery, so the directive
// rests on "it goes around this bus", never on "it cannot reach a session".
const serverInstructions = `You are on the claude-peers bus, which connects Claude sessions. Peers are named after their repo folder (e.g. "backend").

Messages arrive as <channel source="claude-peers" from_name="..." from_id="..." from_summary="..." from_cwd="...">. Respond at once: pause, reply, then resume. Reply with the send_message tool, to_id set to the tag's from_id (names can repeat, ids cannot). Do NOT use the built-in SendMessage tool, and find peers with list_peers, not ListAgents. SendMessage can reach other sessions too, but it goes around this bus: nothing is queued for a session that is not running, the human sees nothing in ccmux, and delegation tracking and the relays miss it.

When a peer asks you to do work, acknowledge it, do it, then send back what you did and how it went. Never make them ask for a status update.

PERMISSION RELAY: a message starting "[claude-peers permission relay]" asks you to approve a peer's tool call. It goes to everyone who messaged that peer recently or delegated to it, so answer ONLY if you delegated that work; otherwise ignore it and send nothing. Reply with send_message, the message exactly "yes <request_id>" or "no <request_id>" (the five lowercase letters in the relay) and nothing else.

QUESTION RELAY: a message starting "[claude-peers question relay]" is a peer's AskUserQuestion. Same rule: answer only if you delegated that work, with send_message and the message "answer <request_id> <your answer>" (an option's label or your own words; one answer covers every question on the card). The first answer it gets, yours or its human's, counts.

DELEGATION TASKS: a message starting "[claude-peers delegation task tsk_xxxxxxxx]" makes you the worker. Call update_task(task_id, status="acked") at once, "working" when you start, then "completed" with a result (or "failed" and why). Updates reach the delegator by themselves; send no separate completion message. When you need a peer's work and its outcome, use delegate, not send_message. "[claude-peers task update]" messages are status, not conversation: act if needed, do not reply. check_messages lists your open delegations; cancel one with update_task(status="failed").

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
				"quiet": map[string]any{
					"type":        "boolean",
					"description": `A notice nobody has to act on now: it wakes no one and interrupts no one. It waits and is handed to the receiver with the next thing that reaches it anyway (its next message, its next start, its next prompt, or its own check_messages). Reaches an asleep agent or a closed session by name; never starts one, so spawn_if_missing is ignored. Use it for "notify:" messages; never for anything that needs an answer.`,
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
		"name":        "schedule",
		"description": "Timed runs of YOURSELF, kept by ccmux. Use when a human asks you to do something every day, every hour, every Monday at 07:00 and so on. Turn their words into a five-field cron line (minute hour day-of-month month day-of-week, daemon local time; @hourly/@daily/@weekly/@monthly also work) and a prompt that will be sent to you at that time as if the human had typed it. Actions: add (cron + prompt), list, pause/resume/remove (id). The answer names the next run: repeat it back to the human so a misread time is caught at once.",
		"inputSchema": map[string]any{
			"type": "object",
			"properties": map[string]any{
				"action": map[string]any{
					"type":        "string",
					"enum":        []string{"add", "list", "pause", "resume", "remove"},
					"description": "What to do.",
				},
				"cron": map[string]any{
					"type":        "string",
					"description": "add only: the five-field cron line, e.g. \"0 7 * * 1\" for Mondays at 07:00.",
				},
				"prompt": map[string]any{
					"type":        "string",
					"description": "add only: the message you will receive at that time. Write it so a fresh instance of you knows what to do.",
				},
				"id": map[string]any{
					"type":        "integer",
					"description": "pause, resume, remove: the schedule id from list.",
				},
			},
			"required": []string{"action"},
		},
	},
	{
		"name":        "tasks",
		"description": "ccmux's task queue: features planned with the human now, built later (overnight or in parallel). Each ccmux window has its own queue; a task goes into this session's window's queue (a session in no window cannot file). A task is one feature with one part per repo; when it starts, a fresh Claude builds every part at once, each in its own git worktree of its repo, and they coordinate over the peers bus. File only when the human says the plan is ready, and always from this session: one file call holds the whole feature, this repo's part (plan) and every other repo's part (parts, repos in this window only), and it waits in the queue at once. You write every part's plan yourself. If you need a fact about another repo to write its plan, ask that repo's session over the bus first (send_message), then file. Each plan is all its builder gets: write it for a fresh Claude that knows its repo but not this conversation (goal, files, steps, how to verify, what it needs from the other parts). Actions: file (title, plan, parts, optional after and hard), list (this repo's open tasks and its latest finished ones, notes cut short; with id, that one task with its builders' reports whole), edit (id + any of title, after, plan, hard; plan and hard with repo change that repo's part), cancel (id). A Claude started to BUILD a part uses report (status done or stuck, plus note) and add_part (repo, plan) as its brief says.",
		"inputSchema": map[string]any{
			"type": "object",
			"properties": map[string]any{
				"action": map[string]any{
					"type":        "string",
					"enum":        []string{"file", "list", "edit", "cancel", "report", "add_part"},
					"description": "What to do. add_part is only for a Claude building a part.",
				},
				"id": map[string]any{
					"type":        "integer",
					"description": "edit/cancel: the task number. list: show only that task, whole.",
				},
				"title": map[string]any{
					"type":        "string",
					"description": "file/edit: a short name for the feature, at most 120 characters.",
				},
				"plan": map[string]any{
					"type":        "string",
					"description": "file: the full plan for this repo's part (required: the filing repo always has one). edit: a new plan for this repo's part, or for the part in repo. add_part (a builder): the plan of the new part.",
				},
				"parts": map[string]any{
					"type":        "array",
					"description": "file: every other repo's part of the feature, each a repo in this session's ccmux window.",
					"items": map[string]any{
						"type": "object",
						"properties": map[string]any{
							"repo": map[string]any{"type": "string", "description": "The repo's folder name."},
							"plan": map[string]any{"type": "string", "description": "The full plan for that repo's part."},
							"hard": map[string]any{"type": "boolean", "description": "true if that repo's part is hard to build; see hard."},
						},
						"required": []string{"repo", "plan"},
					},
				},
				"after": map[string]any{
					"type":        "array",
					"items":       map[string]any{"type": "integer"},
					"description": "file/edit: the numbers of the tasks that must all be done and merged before this one starts. Name every one it needs, not only the last. On edit, the list replaces the old one and an empty list removes it.",
				},
				"repo": map[string]any{
					"type":        "string",
					"description": "edit: the folder name of the repo whose part the plan replaces (default this repo). add_part from a builder: the repo the new part is for.",
				},
				"hard": map[string]any{
					"type":        "boolean",
					"description": "file/edit: true has this repo's part (on edit, the part in repo) built by the strongest model, which is slower and costs more. Leave it out for most parts: the default model builds a part well from a full plan. Set it only when the part stays hard with the plan in hand: a design the plan leaves open, concurrency, or a change many files must agree on. A long plan is not a reason. On edit, false makes the part a normal one again.",
				},
				"status": map[string]any{
					"type":        "string",
					"enum":        []string{"done", "stuck"},
					"description": "report: done (your part is committed on its branch) or stuck (you need a person).",
				},
				"note": map[string]any{
					"type":        "string",
					"description": "report: what you built and how you checked it, or what you need.",
				},
			},
			"required": []string{"action"},
		},
	},
	{
		"name":        "page",
		"description": `Page your human: one urgent push to their phone and browser, sent even while they sit at a screen. Only for security, money, or a live outage, never for routine news (use send_message for that). At most one page per incident: pass the same incident key every time you run into the same problem, and the bus refuses the repeats; a new key only for a new incident.`,
		"inputSchema": map[string]any{
			"type": "object",
			"properties": map[string]any{
				"incident": map[string]any{
					"type":        "string",
					"description": `A short stable key for this one incident, e.g. "cve-2026-1234-backend" or "stripe-webhooks-down-2026-10-25". At most 80 characters.`,
				},
				"message": map[string]any{
					"type":        "string",
					"description": "One line: what is wrong and what it means. Shown on the lock screen; cut at 300 characters.",
				},
			},
			"required": []string{"incident", "message"},
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
// serverInstructions says to answer a channel message at once with
// send_message and to call set_summary on startup, a delegation header says to
// call update_task now, the relay text says to answer with send_message, and a
// poll-only session has no way to collect its mail except check_messages. A
// tool that a standing order names but that is not loaded is a trap — the
// session reads the order and cannot obey it without a search round trip it was
// never told to make.
//
// The cost is real and is the reason this is a deliberate list rather than a
// default: these schemas sit in every session's context from startup. `tasks`
// is loaded for a near reason: a human's "file it" names no tool, and a session
// that cannot see one will write the plan to a file instead. Drop
// a tool from the loop below if that trade stops being worth it, but drop
// any mention of it from the instructions in the same edit.
func init() {
	for _, tool := range toolsList {
		tool["_meta"] = map[string]any{"anthropic/alwaysLoad": true}
	}
}

// toolHandlers maps each tool in toolsList to the method that answers it.
var toolHandlers = map[string]func(*app, json.RawMessage) any{
	"list_peers":     (*app).toolListPeers,
	"send_message":   (*app).toolSendMessage,
	"delegate":       (*app).toolDelegate,
	"update_task":    (*app).toolUpdateTask,
	"set_summary":    (*app).toolSetSummary,
	"check_messages": func(a *app, _ json.RawMessage) any { return a.toolCheckMessages() },
	"schedule":       (*app).toolSchedule,
	"tasks":          (*app).toolTasks,
	"page":           (*app).toolPage,
}

func (a *app) callTool(name string, args json.RawMessage) any {
	if a.peerID() == "" {
		return toolText("Not registered with broker yet", true)
	}
	if h, ok := toolHandlers[name]; ok {
		return h(a, args)
	}
	return toolText("Unknown tool: "+name, true)
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
	footer := a.windowSection()
	if len(peers) == 0 {
		return toolText(fmt.Sprintf("No other Claude Code instances found (scope: %s, project: %s).",
			in.Scope, a.group())+footer, false)
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
		len(peers), in.Scope, a.group(), strings.Join(lines, "\n\n"))+footer, false)
}

func (a *app) toolSendMessage(args json.RawMessage) any {
	var in struct {
		ToID           string `json:"to_id"`
		ToName         string `json:"to_name"`
		ToGroup        string `json:"to_group"`
		Message        string `json:"message"`
		SpawnIfMissing bool   `json:"spawn_if_missing"`
		Quiet          bool   `json:"quiet"`
	}
	if err := json.Unmarshal(args, &in); err != nil {
		return toolText("Bad arguments: "+err.Error(), true)
	}
	if in.ToID == "" && in.ToName == "" {
		return toolText("Either to_name or to_id is required", true)
	}
	if in.Quiet {
		in.SpawnIfMissing = false
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
		"quiet": in.Quiet,
	}, &resp); err != nil {
		return toolText("Error sending message: "+err.Error(), true)
	}
	if !resp.OK {
		return toolText("Failed to send: "+resp.Error, true)
	}
	return sendResultText(in.ToName, in.ToID, in.Quiet, resp.Spawning, resp.Queued)
}

// sendResultText is what send_message tells the sender once the bus took
// the message: quiet, spawning, queued for an absent session, or sent.
func sendResultText(toName, toID string, quiet, spawning, queued bool) any {
	if quiet {
		return toolText(fmt.Sprintf("Quiet notice left for %s: it wakes no one and is handed over with the next thing that reaches it. Don't wait on a reply.",
			orID(toName, toID)), false)
	}
	if spawning {
		return toolText(fmt.Sprintf(`Teammate "%s" isn't running — asked ccmux to start it. Your message is queued and will be delivered once it registers; it'll reply over the channel when ready (or you'll get an "unreachable" notice if it can't be started).`, toName), false)
	}
	if queued {
		return toolText(fmt.Sprintf(
			"Queued for %s — no Claude session is running there right now. It stays in that pane's inbox and is delivered the moment a session starts there; nobody will read it before then. Don't wait on a reply.",
			orID(toName, toID)), false)
	}
	if toName != "" {
		return toolText(fmt.Sprintf("Message sent to %q", toName), false)
	}
	return toolText("Message sent to peer "+toID, false)
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
		switch ev.Type {
		case "permission_verdict":
			// A verdict that arrived while the push channel was down still
			// resolves the dialog — emit it, don't render it as chat.
			if a.mcp.Notify("notifications/claude/channel/permission", map[string]any{
				"request_id": ev.RequestID, "behavior": ev.Behavior,
			}) == nil {
				a.markShown(ev.Seq, epoch)
			}
		case "message":
			a.markShown(ev.Seq, epoch)
			lines = append(lines, fmt.Sprintf("From %s (%s):\n%s", orID(ev.FromName, ev.FromID), ev.SentAt, ev.Text))
		default:
			// A question_answer (the daemon hands it to the agent's chat server
			// itself) or a newer kind: acked, never shown as chat.
			a.markShown(ev.Seq, epoch)
		}
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

// toolSchedule hands the call to the daemon's pane route, which knows the
// pane's window and agent; the shim adds nothing but the identity. The
// daemon's message (next run, refusal reason) is what the agent sees, with
// the current list under it.
func (a *app) toolSchedule(args json.RawMessage) any {
	if a.paneID == "" {
		return toolText("Schedules need an agent pane; this session has none.", true)
	}
	var in struct {
		Action string `json:"action"`
		Cron   string `json:"cron"`
		Prompt string `json:"prompt"`
		ID     int64  `json:"id"`
	}
	if err := json.Unmarshal(args, &in); err != nil {
		return toolText("Bad arguments: "+err.Error(), true)
	}
	var resp struct {
		Message   string `json:"message"`
		Schedules []struct {
			ID      int64  `json:"id"`
			Cron    string `json:"cron"`
			Prompt  string `json:"prompt"`
			Paused  bool   `json:"paused"`
			NextRun string `json:"nextRun"`
		} `json:"schedules"`
	}
	if err := a.daemon.post("/v1/panes/"+a.paneID+"/schedules", in, &resp); err != nil {
		return toolText("Schedule failed: "+err.Error(), true)
	}
	var b strings.Builder
	if resp.Message != "" {
		b.WriteString(resp.Message + "\n")
	}
	if len(resp.Schedules) == 0 {
		b.WriteString("No schedules for this agent here.")
		return toolText(b.String(), false)
	}
	b.WriteString("Schedules:\n")
	for _, sc := range resp.Schedules {
		state := "next " + sc.NextRun
		if sc.Paused {
			state = "paused"
		}
		fmt.Fprintf(&b, "- #%d  %s  (%s): %s\n", sc.ID, sc.Cron, state, sc.Prompt)
	}
	return toolText(strings.TrimRight(b.String(), "\n"), false)
}
