package main

import "ccmux.dev/ccmuxd/internal/buscontext"

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

// sessions fetches the repo sessions of this session's window; nil on any
// failure, like agents.
func (a *app) sessions() []sessionEntry {
	var out []sessionEntry
	a.windowList("/v1/peers/sessions", &out)
	return out
}

func (a *app) windowList(path string, out any) {
	id := a.peerID()
	if id == "" || a.daemon == nil {
		return
	}
	_ = a.daemon.post(path, map[string]any{"peer_id": id}, out)
}

// windowParagraph is what initialize appends after the frozen server
// instructions: the window's repo sessions, then its agents. An agent's own
// folder holds only its memory; this paragraph is how it learns where the
// project's code is without anyone writing paths into its instructions.
func (a *app) windowParagraph() string {
	return buscontext.Paragraph(a.sessions(), a.agents())
}

// windowSection is the list_peers footer: the same two lists, tool-shaped.
func (a *app) windowSection() string {
	return buscontext.Section(a.sessions(), a.agents())
}
