package api

import (
	"net/http"

	"ccmux.dev/ccmuxd/internal/buscontext"
	"ccmux.dev/ccmuxd/internal/manager"
)

// paneBusContext: GET /v1/panes/{id}/bus-context → the window paragraph for
// the pane's session as text: its repo sessions, then its agents. This is
// the opencode carrier: opencode ignores an MCP server's instructions, so
// the ccmux plugin fetches this at every turn and appends it to the system
// prompt (Claude Code gets the same text through the peers shim). Loopback
// only, like agent-signal: the plugin runs on this host with the pane's own
// environment. A pane outside any window answers an empty body; an
// unreadable window table is a refusal, never "nothing here".
func (s *Server) paneBusContext(w http.ResponseWriter, r *http.Request) {
	if !requireLoopback(w, r) {
		return
	}
	text, status, msg := s.busContextOfPane(r.PathValue("id"))
	if msg != "" {
		writeError(w, status, msg)
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Write([]byte(text))
}

func (s *Server) busContextOfPane(paneID string) (string, int, string) {
	group, ok := s.mgr.GroupForPane(paneID)
	if !ok || group == "" {
		return "", 0, ""
	}
	win, status, msg := s.windowByName(group)
	if status == http.StatusNotFound {
		return "", 0, ""
	}
	if msg != "" {
		return "", status, msg
	}
	agents, err := s.windowAgentsOf(win)
	if err != nil {
		return "", http.StatusInternalServerError, err.Error()
	}
	return buscontext.Paragraph(s.windowSessions(win), s.sharedDirOf(win), agents), 0, ""
}

// windowAgentsOf is windowInstances in the renderer's shape; no agent store
// means no agents, not an error.
func (s *Server) windowAgentsOf(win manager.WindowInfo) ([]buscontext.Agent, error) {
	if s.agents == nil {
		return nil, nil
	}
	list, err := s.windowInstances(win)
	if err != nil {
		return nil, err
	}
	out := make([]buscontext.Agent, 0, len(list))
	for _, a := range list {
		out = append(out, buscontext.Agent{Name: a.Name, Icon: a.Icon, Description: a.Description, State: a.State})
	}
	return out, nil
}
