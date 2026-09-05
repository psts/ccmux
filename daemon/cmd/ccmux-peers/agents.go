package main

import (
	"fmt"
	"strings"
)

// agentEntry mirrors the daemon's agent-instance view for the caller's
// workspace (POST /v1/peers/agents).
type agentEntry struct {
	Name        string `json:"name"`
	Icon        string `json:"icon"`
	Description string `json:"description"`
	State       string `json:"state"` // absent | asleep | running
}

// agents fetches the base agents as seen from this session's workspace; nil
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
