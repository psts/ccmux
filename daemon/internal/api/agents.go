package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"

	"ccmux.dev/ccmuxd/internal/agent"
	"ccmux.dev/ccmuxd/internal/manager"
	"ccmux.dev/ccmuxd/internal/peers"
)

// SetAgents wires the base-agent folder store. Without it the /v1/agents
// routes answer 503, which is how a lens learns the daemon predates agents.
func (s *Server) SetAgents(st *agent.Store, maxRunning int) {
	s.agents, s.agentsMax = st, maxRunning
	s.mgr.Agents = st
	s.mgr.AgentsMax = maxRunning
	s.mgr.WakeAgent = s.wakeAgent
	s.wirePeersAgents()
}

const agentsUnavailable = "agents are not available on this daemon"

// agentsReady answers the 503 for a daemon without the agent store; "" on
// the other error paths means "the store is there, something else broke",
// which the lenses show by message instead of hiding the feature.
func (s *Server) agentsReady(w http.ResponseWriter) bool {
	if s.agents == nil {
		writeError(w, http.StatusServiceUnavailable, agentsUnavailable)
		return false
	}
	return true
}

// wirePeersAgents gives the bus its agent hooks once BOTH the store and the
// peers service are present. Called from SetAgents and EnablePeers so main's
// wiring order does not matter (it bit once: agents were wired first and the
// hooks landed on a nil service, so contact fell through to the repo guess).
func (s *Server) wirePeersAgents() {
	if s.peersSvc == nil || s.agents == nil {
		return
	}
	s.peersSvc.IsAgent = s.isAgent
	s.peersSvc.StartAgent = s.startAgentForPeer
	s.peersSvc.WakePane = s.wakePane
	s.peersSvc.PushToPane = func(paneID, text string) error {
		err := s.mgr.PushToAgentPane(paneID, text)
		switch {
		case errors.Is(err, manager.ErrNotAgentPane):
			return peers.ErrNoPanePush
		case errors.Is(err, manager.ErrAgentAsleep):
			return peers.ErrAgentAsleep
		}
		return err
	}
}

// wakeAgent is the lifecycle loop's restart path for a keep-alive instance
// found asleep: the one start flow, from the CURRENT base, with no prompt.
func (s *Server) wakeAgent(wsID, name string) error {
	out := s.startAgent(wsID, name, "", nil)
	switch {
	case out.msg == "":
		return nil
	case out.err != nil:
		return out.err // keeps ErrAgentRunning / ErrAgentCap for the backoff to read
	}
	return errors.New(out.msg)
}

// wakePane is the bus's answer to a message for an asleep opencode instance:
// start it with that message as its first prompt.
func (s *Server) wakePane(paneID, text string) error {
	wsID := s.mgr.WorkspaceForPane(paneID)
	ws := s.mgr.Workspace(wsID)
	if ws == nil {
		return errors.New("unknown pane")
	}
	for _, p := range ws.Panes {
		if p.ID == paneID && p.Agent != "" {
			out := s.startAgent(wsID, p.Agent, text, nil)
			if errors.Is(out.err, manager.ErrAgentRunning) {
				// Another start is in flight with ITS prompt; this message is
				// not in it. The bus retries the TUI push once the harness is up.
				return peers.ErrStartInFlight
			}
			return peerStartError(out)
		}
	}
	return errors.New("not an agent pane")
}

// listAgents: GET /v1/agents → every base definition, instructions included.
// An unreadable base is a 503 with its name, never a silently shorter list.
func (s *Server) listAgents(w http.ResponseWriter, r *http.Request) {
	if !s.agentsReady(w) {
		return
	}
	defs, err := s.agents.List()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error()) // names the broken base; 503 is "no agents support"
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"agents": defs})
}

// putAgent: PUT /v1/agents/{name} creates or updates ONE base. Deliberately
// not a whole-list replace like the other settings objects: a base folder
// holds skills a human wrote, so removing one must be the explicit DELETE,
// never the side effect of a list that omitted it.
func (s *Server) putAgent(w http.ResponseWriter, r *http.Request) {
	if !s.agentsReady(w) {
		return
	}
	name := r.PathValue("name")
	if !agent.ValidName(name) {
		writeError(w, http.StatusBadRequest, "agent name: use 3 to 24 chars of a-z, 0-9 and -")
		return
	}
	// Merge, not replace: the request carries the fields a lens edits, and
	// json.Unmarshal into the STORED definition leaves every other field as it
	// was — a hand-tuned permission or a pinned model survives a blur-save.
	d, err := s.agents.Get(name)
	if errors.Is(err, agent.ErrNotFound) {
		d = agent.Defaults() // a new agent starts from the defaults, request on top
	} else if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 1<<20))
	if err != nil || json.Unmarshal(body, &d) != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	d.Name = name
	if msg := s.rejectDefinition(d); msg != "" {
		writeError(w, http.StatusBadRequest, msg)
		return
	}
	stored, err := s.agents.Save(d)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if err := s.renameInstances(stored); err != nil {
		// The base is saved; what failed is the part the user will look for
		// in the sidebar, so say so instead of answering 200.
		writeError(w, http.StatusInternalServerError, "base saved, but its instance sessions were not renamed: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, stored)
}

// renameInstances keeps every instance session named after the base's
// current icon and name, so an icon changed in the editor shows in the
// sidebars at once, without a restart. The first failure is returned; the
// rest are still attempted. A session killed between the listing and its
// rename is nothing to report.
func (s *Server) renameInstances(d agent.Definition) error {
	want := agentSessionName(d)
	var first error
	for _, ws := range s.mgr.List() {
		if ws.Agent != d.Name {
			continue
		}
		err := s.mgr.RenameWorkspace(ws.ID, want)
		if err != nil && !errors.Is(err, manager.ErrUnknownWorkspace) && first == nil {
			first = fmt.Errorf("session %s: %w", ws.ID, err)
		}
	}
	return first
}

// rejectDefinition is every reason a definition is refused before anything
// is written: the shape rules, then the harness registry.
func (s *Server) rejectDefinition(d agent.Definition) string {
	if msg := agent.Reject(d); msg != "" {
		return msg
	}
	return s.rejectAgentHarness(d.Harness)
}

// rejectAgentHarness refuses a harness name the registry cannot resolve, so
// an agent never points at a harness that would 400 at start time. Empty
// means the default and is always fine.
func (s *Server) rejectAgentHarness(name string) string {
	if name == "" || s.mgr.Harnesses == nil {
		return ""
	}
	if _, err := s.mgr.Harnesses.Resolve(name); err != nil {
		return "agent harness: " + err.Error()
	}
	return ""
}

// deleteAgent: DELETE /v1/agents/{name} removes the base folder, skills
// included. The lens confirms with the human before calling.
func (s *Server) deleteAgent(w http.ResponseWriter, r *http.Request) {
	if !s.agentsReady(w) {
		return
	}
	err := s.agents.Delete(r.PathValue("name"))
	switch {
	case errors.Is(err, agent.ErrNotFound):
		writeError(w, http.StatusNotFound, "no such agent")
	case err != nil:
		writeError(w, http.StatusInternalServerError, err.Error())
	default:
		log.Printf("agent %q deleted", r.PathValue("name"))
		w.WriteHeader(http.StatusNoContent)
	}
}
