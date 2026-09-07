package main

import (
	"fmt"
	"strings"
)

// agentEntry mirrors the daemon's agent-instance view for the caller's
// window (POST /v1/peers/agents).
type agentEntry struct {
	Name        string `json:"name"`
	Icon        string `json:"icon"`
	Description string `json:"description"`
	State       string `json:"state"` // absent | asleep | running
}

// agents fetches the base agents as seen from this session's window; nil
// on any failure — the catalog is advisory, never a reason to fail a tool.
func (a *app) agents() []agentEntry {
	id := a.peerID()
	if id == "" || a.daemon == nil {
		return nil
	}
	var out []agentEntry
	if err := a.daemon.post("/v1/peers/agents", map[string]any{"peer_id": id}, &out); err != nil {
		return nil
	}
	return out
}

// agentsParagraph is appended AFTER serverInstructions at initialize. The
// frozen wording above it is untouched; this is the one addition, and it
// says how an agent differs from a teammate: it exists before it runs, and
// naming it starts it in this project.
func agentsParagraph(list []agentEntry) string {
	if len(list) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("\n\nAGENTS ON THIS BUS: role agents that exist as definitions and start when contacted. ")
	b.WriteString("Message one with send_message(to_name=<agent>, spawn_if_missing=true); if it is asleep or not yet in this project it starts here and gets your message. ")
	b.WriteString("Use delegate for work whose completion you must know about. list_peers shows their live state.\n")
	for _, e := range list {
		b.WriteString(agentLine(e))
	}
	return b.String()
}

func agentLine(e agentEntry) string {
	state := e.State
	if state == "absent" {
		state = "not in this project yet; starts on contact"
	}
	return fmt.Sprintf("- %s: %s [%s]\n", e.Name, e.Description, state)
}

// agentsSection is the list_peers footer.
func agentsSection(list []agentEntry) string {
	if len(list) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("\n\nAgents (message by name with spawn_if_missing=true to start):\n")
	for _, e := range list {
		b.WriteString(agentLine(e))
	}
	return b.String()
}

// sessionEntry mirrors the daemon's view of one repo session in the
// caller's window (POST /v1/peers/sessions).
type sessionEntry struct {
	Name     string `json:"name"`
	RepoPath string `json:"repoPath"`
	Status   string `json:"status"` // live | cold
}

// sessions fetches the repo sessions of this session's window; nil on any
// failure, like agents: advisory, never a reason to fail a tool.
func (a *app) sessions() []sessionEntry {
	id := a.peerID()
	if id == "" || a.daemon == nil {
		return nil
	}
	var out []sessionEntry
	if err := a.daemon.post("/v1/peers/sessions", map[string]any{"peer_id": id}, &out); err != nil {
		return nil
	}
	return out
}

// windowParagraph is what initialize appends after the frozen server
// instructions: the window's repo sessions, then its agents. An agent's own
// folder holds only its memory; this paragraph is how it learns where the
// project's code is without anyone writing paths into its instructions.
func (a *app) windowParagraph() string {
	return sessionsParagraph(a.sessions()) + agentsParagraph(a.agents())
}

// windowSection is the list_peers footer: the same two lists, tool-shaped.
func (a *app) windowSection() string {
	return sessionsSection(a.sessions()) + agentsSection(a.agents())
}

func sessionsParagraph(list []sessionEntry) string {
	if len(list) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("\n\nPROJECT SESSIONS IN THIS WINDOW: the repos this project is made of, each its own session on this host. ")
	b.WriteString("Read their files directly at the folder. To ask a session something, message it by name; ")
	b.WriteString("if list_peers does not show it, send with spawn_if_missing=true and it starts.\n")
	for _, e := range list {
		b.WriteString(sessionLine(e))
	}
	return b.String()
}

func sessionsSection(list []sessionEntry) string {
	if len(list) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("\n\nProject sessions in this window (folders on this host; message by name, spawn_if_missing=true starts one that is not listed above):\n")
	for _, e := range list {
		b.WriteString(sessionLine(e))
	}
	return b.String()
}

func sessionLine(e sessionEntry) string {
	state := e.Status
	if state == "cold" {
		state = "archived; starts on contact"
	}
	return fmt.Sprintf("- %s: %s [%s]\n", e.Name, e.RepoPath, state)
}
