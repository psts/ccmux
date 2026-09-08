// Package buscontext renders what every session in a shared window is told
// about that window: the repo sessions it is made of and the role agents
// that can be reached there. One renderer, two carriers: the peers shim
// appends Paragraph to its MCP instructions (which Claude Code reads) and
// Section to list_peers; the daemon serves Paragraph on a pane-keyed route
// for the opencode plugin, because opencode ignores MCP instructions.
package buscontext

import (
	"fmt"
	"strings"
)

// Session is one repo session of the window: the name it is messaged by,
// where its code is on this host, and whether its tmux session is open
// ("live") or archived ("cold"). Whether someone is listening there is
// list_peers' question, not this one's.
type Session struct {
	Name     string `json:"name"`
	RepoPath string `json:"repoPath"`
	Status   string `json:"status"`
}

// Agent is one base agent as seen from the window: absent (never added
// there), asleep, or running.
type Agent struct {
	Name        string `json:"name"`
	Icon        string `json:"icon"`
	Description string `json:"description"`
	State       string `json:"state"`
}

// Paragraph is the instructions form: sessions first, then the window's
// shared folder (shared is its path, "" before any agent has started
// there), then agents. Empty when there is nothing to say, so a caller
// outside any window appends nothing.
func Paragraph(sessions []Session, shared string, agents []Agent) string {
	return sessionsParagraph(sessions) + sharedParagraph(shared) + agentsParagraph(agents)
}

// Section is the list_peers footer: the same lists, tool-shaped.
func Section(sessions []Session, shared string, agents []Agent) string {
	return sessionsSection(sessions) + sharedSection(shared) + agentsSection(agents)
}

func sharedParagraph(dir string) string {
	if dir == "" {
		return ""
	}
	return "\n\nSHARED FOLDER of this window: " + dir + ". Every agent here reads and writes it: put what another agent in this project needs there, " +
		"as dated files with frontmatter inside a folder named after you (the shared folder's own README.md has the layout). " +
		"Its .env is loaded into every agent's environment at its next start: one KEY=VALUE per line, the value taken as-is (surrounding quotes stripped, no shell syntax, nothing runs); an agent's own folder may hold a .env too, and that one wins. Secrets go in .env, never in notes or memory.\n"
}

func sharedSection(dir string) string {
	if dir == "" {
		return ""
	}
	return "\n\nShared folder of this window (every agent reads and writes it; its .env loads into agents at start): " + dir + "\n"
}

func sessionsParagraph(list []Session) string {
	if len(list) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("\n\nPROJECT SESSIONS IN THIS WINDOW: the repos this project is made of, each its own session on this host. ")
	b.WriteString("Their code lives at these folders: read there if your harness reaches it, otherwise message the session by name. ")
	b.WriteString("If list_peers does not show it, send with spawn_if_missing=true and it starts.\n")
	for _, e := range list {
		b.WriteString(sessionLine(e))
	}
	return b.String()
}

func sessionsSection(list []Session) string {
	if len(list) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("\n\nProject sessions in this window (folders on this host; message by name, spawn_if_missing=true starts one not listed above):\n")
	for _, e := range list {
		b.WriteString(sessionLine(e))
	}
	return b.String()
}

func sessionLine(e Session) string {
	state := e.Status
	if state == "cold" {
		state = "archived; starts on contact"
	}
	return fmt.Sprintf("- %s: %s [%s]\n", e.Name, e.RepoPath, state)
}

// agentsParagraph says how an agent differs from a teammate: it exists
// before it runs, and naming it starts it in this project.
func agentsParagraph(list []Agent) string {
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

func agentsSection(list []Agent) string {
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

func agentLine(e Agent) string {
	state := e.State
	if state == "absent" {
		state = "not in this project yet; starts on contact"
	}
	return fmt.Sprintf("- %s: %s [%s]\n", e.Name, e.Description, state)
}
