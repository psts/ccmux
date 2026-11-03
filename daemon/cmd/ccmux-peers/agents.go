package main

import (
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

// windowParagraph is what initialize appends after the frozen server
// instructions: the window's repo sessions, its shared folder, then its
// agents. An agent's own
// folder holds only its memory; this paragraph is how it learns where the
// project's code is without anyone writing paths into its instructions.
func (a *app) windowParagraph() string {
	return a.windowParagraphOf(a.sessions())
}

func (a *app) windowParagraphOf(w windowSessions) string {
	return buscontext.Paragraph(w.Reader, w.Sessions, w.Shared, a.agents())
}

// instructionsCap is how much of a server's instructions Claude Code keeps;
// anything past it never reaches the session.
const instructionsCap = 4096

// ownsPaneBus is isBusOwner, swapped in tests (a test run has no pane).
var ownsPaneBus = isBusOwner

// registerWait is how long the handshake waits for the first registration:
// before it, the daemon cannot say which window this session is in, and the
// per-session parts would be left out for the whole session.
const registerWait = 2 * time.Second

// instructions is what initialize and server/discover send: the fixed rules,
// then the task-queue rule, then the window's lists, in that order, so the
// rules are always inside instructionsCap and only the window's lists are cut,
// from the end: in a window with a shared folder that is often its notes and
// the agents list. The log says when.
func (a *app) instructions() string {
	// Only the pane's own session ever registers (a subagent or a warm spare
	// sharing the pane never does), and only the first handshake request
	// waits: server/discover then initialize must not pay it twice.
	if a.paneID != "" && ownsPaneBus() {
		a.waitOnce.Do(func() { a.waitRegistered(registerWait) })
	}
	w := a.sessions()
	out := serverInstructions + a.taskQueueParagraph(w) + a.windowParagraphOf(w)
	if len(out) > instructionsCap {
		logf("instructions are %d chars and Claude Code keeps %d: the end of this window's lists does not reach the session", len(out), instructionsCap)
	}
	return out
}

// taskQueueParagraph is the build-or-file rule, only for a session that can
// file a task: a ccmux pane, in a window (the daemon lists no sessions
// outside one), and a repo's session, not a role agent's. Anywhere else the
// choice's second half would fail, and the human's answer would be wasted.
func (a *app) taskQueueParagraph(w windowSessions) string {
	if a.paneID == "" || len(w.Sessions) == 0 || w.Reader.Agent {
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
