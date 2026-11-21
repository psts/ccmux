package buscontext

import (
	"fmt"
	"strings"
)

// Fit is Paragraph for a carrier that keeps only room bytes of it: the peers
// shim, whose MCP instructions Claude Code cuts at a fixed length. A cut
// there lands mid-list and tells the session nothing about what it lost (in
// the logs of 2026-11-14 that was 263 sessions of 269: the shared folder and
// every agent, in each). So the lists are shortened here instead, and every
// shortened form says that list_peers has the rest: its Section is never
// shortened.
//
// The forms are tried in order and the first that fits is sent; each gives
// up the least useful thing still left. What each agent does goes first,
// then the agents' names, then the end of each owns line a step at a time,
// then the owns lines, then the sessions' names. The reader's rule is in
// every form. When not even the last one fits it is sent anyway, and the
// carrier cuts a line that only points at list_peers.
//
// room is counted in bytes, which is never fewer than the characters a
// carrier counts, so a text that fits here fits there.
func Fit(r Reader, sessions []Session, shared string, agents []Agent, room int) string {
	out := Paragraph(r, sessions, shared, agents)
	for _, f := range shorterForms {
		if len(out) <= room {
			break
		}
		out = sessionsShort(r, sessions, f.owns) + sharedShort(shared) + agentsShort(agents, f.agentNames)
	}
	return out
}

// What a shortened form keeps of each session: every rune of its owns line,
// none of it (the names, on one line), not even the name (how many), or the
// first so many runes.
const (
	ownsAll   = -1
	ownsNone  = 0
	namesGone = -2
)

// shorterForms is Fit's order. Owns lines outlast the agents' names because
// they are how a session knows whose work a request is; a role agent is
// asked for by name, and list_peers has the names.
var shorterForms = []struct {
	owns       int
	agentNames bool
}{
	{ownsAll, true},
	{ownsAll, false},
	{64, false},
	{40, false},
	{24, false},
	{ownsNone, true},
	{namesGone, false},
}

// The rules of a shortened form. A repo session's leaves out how to report
// on a delegation, which the shim's own rules say to every session; a role
// agent's sends it to list_peers once the folders are not in the text.
const (
	ownerRuleShort = "Each repo is owned by its own session. Never read or change another session's repo: " +
		"when your task needs work in one, delegate it to that session (delegate with to_name=<session>, spawn_if_missing=true) " +
		"saying what you need and why, then carry on with your own part. " +
		"When one delegates work to you and something is unclear, ask it with send_message.\n"
	mainRuleShort = "You are this repo's main session. Never read or change another repo yourself: message it by its plain name (to_name=<repo>), " +
		"which reaches a helper of your own there that reads, checks and answers but never lands a change. Real work in another repo is a task part.\n"
	agentLead          = "The project's repos, one session each. "
	agentRuleNoFolders = "list_peers shows each one's folder: read there if your harness reaches it, " +
		"otherwise message the session by name, with spawn_if_missing=true if it is not running.\n"
)

func shortRule(r Reader, owns int) string {
	switch {
	case r.Main:
		return mainRuleShort
	case !r.Agent:
		return ownerRuleShort
	case owns == ownsNone || owns == namesGone:
		return agentLead + agentRuleNoFolders
	}
	return agentLead + agentRule
}

func sessionsShort(r Reader, list []Session, owns int) string {
	if len(list) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("\n\nPROJECT SESSIONS IN THIS WINDOW: ")
	b.WriteString(shortRule(r, owns))
	switch owns {
	case namesGone:
		fmt.Fprintf(&b, "Sessions: %d (list_peers names each and says what it owns).\n", len(list))
	case ownsNone:
		names := make([]string, 0, len(list))
		for _, e := range list {
			names = append(names, sessionName(r, e))
		}
		b.WriteString("Sessions (list_peers says what each owns): " + strings.Join(names, ", ") + ".\n")
	default:
		b.WriteString(sessionLines(r, list, owns))
	}
	return b.String()
}

// sessionLines is one line per session, the owns line cut to owns runes
// unless it is ownsAll.
func sessionLines(r Reader, list []Session, owns int) string {
	var b strings.Builder
	for _, e := range list {
		if owns != ownsAll {
			e.Owns = clip(e.Owns, owns)
		}
		b.WriteString(sessionLine(r, e))
	}
	if owns != ownsAll {
		b.WriteString("list_peers has each line whole.\n")
	}
	return b.String()
}

// clip keeps the first n runes of s and marks the cut.
func clip(s string, n int) string {
	runes := []rune(s)
	if len(runes) <= n {
		return s
	}
	return strings.TrimRight(string(runes[:n]), " ,;") + "…"
}

func sharedShort(dir string) string {
	if dir == "" {
		return ""
	}
	return "\n\nSHARED FOLDER of this window: " + dir + ". Its README.md says how it is used.\n"
}

func agentsShort(list []Agent, names bool) string {
	if len(list) == 0 {
		return ""
	}
	if !names {
		return fmt.Sprintf("\n\nAGENTS ON THIS BUS: %d (role agents; list_peers names each and says what it does).\n", len(list))
	}
	all := make([]string, 0, len(list))
	for _, e := range list {
		all = append(all, e.Name)
	}
	return "\n\nAGENTS ON THIS BUS: role agents, started by a message with to_name=<agent> and spawn_if_missing=true; " +
		"list_peers says what each does. Names: " + strings.Join(all, ", ") + ".\n"
}
