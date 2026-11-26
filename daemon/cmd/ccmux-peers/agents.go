package main

import (
	"os"
	"time"

	"ccmux.dev/ccmuxd/internal/buscontext"
)

// agentEntry mirrors the daemon's agent-instance view for the caller's
// window (POST /v1/peers/agents); sessionEntry its repo sessions (POST
// /v1/peers/sessions). Both decode straight into the shared renderer's types.
type (
	agentEntry   = buscontext.Agent
	sessionEntry = buscontext.Session
)

// agents fetches the base agents as seen from this session's window; nil
// on any failure — the catalog is advisory, never a reason to fail a tool.
func (a *app) agents() []agentEntry {
	var out []agentEntry
	a.windowList("/v1/peers/agents", &out)
	return out
}

// windowSessions is the daemon's answer to POST /v1/peers/sessions: the
// window's repo sessions, its shared folder ("" before it exists) and who
// this session is to them. A daemon too old to say reads as a repo
// session, which is told to hand other repos' work to their owners.
type windowSessions struct {
	Sessions []sessionEntry    `json:"sessions"`
	Shared   string            `json:"shared"`
	Reader   buscontext.Reader `json:"reader"`
}

// sessions fetches the repo sessions and shared folder of this session's
// window; empty on any failure, like agents.
func (a *app) sessions() windowSessions {
	var out windowSessions
	a.windowList("/v1/peers/sessions", &out)
	return out
}

func (a *app) windowList(path string, out any) {
	id := a.peerID()
	if id == "" || a.daemon == nil {
		return
	}
	if err := a.daemon.post(path, map[string]any{"peer_id": id}, out); err != nil {
		logf("%s: %v (the window context is left out)", path, err)
	}
}

// windowLists is what initialize appends after the rules: the window's repo
// sessions, its shared folder, then its agents. An agent's own folder holds
// only its memory; this is how it learns where the project's code is without
// anyone writing paths into its instructions. room is what the rules left of
// instructionsCap: the lists go out in the longest form that fits it
// (buscontext.Fit), and the log says when that was not the whole one.
func (a *app) windowLists(w windowSessions, room int) string {
	agents := a.agents()
	out := buscontext.Fit(w.Reader, w.Sessions, w.Shared, agents, room)
	switch whole := len(buscontext.Paragraph(w.Reader, w.Sessions, w.Shared, agents)); {
	case len(out) > room:
		logf("this window's lists are %d chars at their shortest and the rules leave %d of the %d Claude Code keeps: their end does not reach the session", len(out), room, instructionsCap())
	case len(out) < whole:
		logf("this window's lists went out shortened, %d chars of %d, to fit the %d Claude Code keeps: list_peers has them whole", len(out), whole, instructionsCap())
	}
	return out
}

// instructionsCap is how much of a server's instructions Claude Code keeps;
// anything past it never reaches the session. Claude Code takes the length
// from its environment, which it starts this process with, so the shim reads
// the same variable: raised in a pane ccmux started, Claude Code's own
// default anywhere else (buscontext.Cap).
func instructionsCap() int {
	return buscontext.Cap(os.Getenv(buscontext.CapEnv))
}

// ownsPaneBus is isBusOwner, swapped in tests (a test run has no pane).
var ownsPaneBus = isBusOwner

// registerWait is how long the handshake waits for the first registration:
// before it, the daemon cannot say which window this session is in, and the
// per-session parts would be left out for the whole session.
const registerWait = 2 * time.Second

// instructions is what initialize and server/discover send: the fixed rules,
// then the task-queue rule and the to-do rule, then the window's lists, so the
// rules are always inside instructionsCap. The lists get what is left of it,
// made shorter to fit rather than cut by Claude Code (windowLists).
func (a *app) instructions() string {
	// Only the pane's own session ever registers (a subagent or a warm spare
	// sharing the pane never does), and only the first handshake request
	// waits: server/discover then initialize must not pay it twice.
	if a.paneID != "" && ownsPaneBus() {
		a.waitOnce.Do(func() { a.waitRegistered(registerWait) })
	}
	w := a.sessions()
	rules := serverInstructions + a.taskQueueParagraph(w) + a.todoParagraph(w)
	return rules + a.windowLists(w, instructionsCap()-len(rules))
}

// todoParagraph is the to-do rule, for a session that has a repo's list to
// add to: a ccmux pane, and not a role agent's (its folder is no repo, and
// no row on the thread page shows a list for it).
func (a *app) todoParagraph(w windowSessions) string {
	if a.paneID == "" || w.Reader.Agent {
		return ""
	}
	return todoRule
}

// todoRule is what makes the to-do lists fill by themselves: every repo
// session is told, once, to add what it notices and leaves, with no human
// asking each time. The tool's own description says it too (todoToolText);
// this is the standing order.
const todoRule = `

TO-DO: When you notice something you are not fixing now (an "Also found", a finding you leave, a follow-up, a bug on the way), add it with the todo tool, unasked. Urgency: high if it breaks things or loses data, medium if it will bite soon, else low. List first to avoid repeats; mark an item done when you fix it.`

// taskQueueParagraph is the build-or-file rule, only for a session that can
// file a task: a ccmux pane, in a window (the daemon lists no sessions
// outside one), and a repo's session, not a role agent's. Anywhere else the
// choice's second half would fail, and the human's answer would be wasted.
// A repo's planner is not asked either: it always files and never builds
// (its brief says so), and the person chose "file" by pressing Make task.
func (a *app) taskQueueParagraph(w windowSessions) string {
	if a.paneID == "" || len(w.Sessions) == 0 || w.Reader.Agent || w.Reader.Planner {
		return ""
	}
	return taskQueueRule
}

// taskQueueRule tells a repo session to offer the task queue before it
// builds what it planned. Without it, the last answer to a planning
// question started the build, and filing it meant cancelling and asking
// again. In plan mode the route is spelled out: approving a plan starts
// the build, so "file it" must never go through a plain approval.
const taskQueueRule = `

TASK QUEUE: When you have planned a change with the human (asked them about it, or shown a plan) and are about to build it, make the last question of your final AskUserQuestion "Build it now, or file it as a task?". Build it now: go ahead here. File it as a task: call the tasks tool (action file) with the whole feature, this repo's part as plan and every other repo's in parts, tell the human its number, and stop without building. In plan mode ask before ExitPlanMode; to file, call the tasks tool still in plan mode, and only if that is refused, ExitPlanMode with the plan "File this as a task; do not build it", then file and stop. Skip this for a quick change the human said to just do, when building a task's part, or when working a delegation.`

// windowSection is the list_peers footer: the same lists, tool-shaped.
func (a *app) windowSection() string {
	w := a.sessions()
	return buscontext.Section(w.Reader, w.Sessions, w.Shared, a.agents())
}
