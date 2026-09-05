package api

import (
	"context"
	"errors"
	"log"
	"net/http"
	"strconv"
	"time"

	"ccmux.dev/ccmuxd/internal/agent"
	"ccmux.dev/ccmuxd/internal/harness"
	"ccmux.dev/ccmuxd/internal/manager"
	"ccmux.dev/ccmuxd/internal/model"
)

// agentInstance is one base agent as seen from one workspace: the base's
// identity plus whether it has been added here and what it is doing.
type agentInstance struct {
	Name        string `json:"name"`
	Icon        string `json:"icon"`
	Description string `json:"description"`
	Version     string `json:"version"`
	// State is "absent" (never added to this workspace), "asleep" (its pane
	// sits at a shell; a message starts it) or "running".
	State string `json:"state"`
	Pane  string `json:"pane,omitempty"`
	// PaneVersion is the base version the instance last started with; Drift
	// says the base has moved since — a restart picks the new one up.
	PaneVersion string `json:"paneVersion,omitempty"`
	Drift       bool   `json:"drift,omitempty"`
}

// listWorkspaceAgents: GET /v1/workspaces/{id}/agents.
func (s *Server) listWorkspaceAgents(w http.ResponseWriter, r *http.Request) {
	ws := s.mgr.Workspace(r.PathValue("id"))
	if ws == nil {
		writeError(w, http.StatusNotFound, "unknown workspace")
		return
	}
	if s.agents == nil {
		writeError(w, http.StatusServiceUnavailable, "agents are not available on this daemon")
		return
	}
	defs, err := s.agents.List()
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, err.Error())
		return
	}
	out := make([]agentInstance, 0, len(defs))
	for _, d := range defs {
		out = append(out, s.instanceOf(ws.ID, d))
	}
	writeJSON(w, http.StatusOK, map[string]any{"agents": out})
}

func (s *Server) instanceOf(wsID string, d agent.Definition) agentInstance {
	inst := agentInstance{Name: d.Name, Icon: d.Icon, Description: d.Description, Version: d.Version, State: "absent"}
	p := s.mgr.AgentPane(wsID, d.Name)
	if p == nil {
		return inst
	}
	inst.Pane, inst.PaneVersion, inst.Drift = p.ID, p.AgentVersion, p.AgentVersion != d.Version
	inst.State = "running"
	if s.mgr.PaneAtShell(p.ID) {
		inst.State = "asleep"
	}
	return inst
}

type startAgentReq struct {
	Prompt    string `json:"prompt"`
	CreatedBy string `json:"createdBy"`
}

// startWorkspaceAgent: POST /v1/workspaces/{id}/agents/{name} adds the base
// to the workspace (instance folder bootstrapped once, opencode config
// regenerated) and starts it with the prompt as its first message, or wakes
// an asleep instance the same way. 409 when the instance is already running
// (type into its pane instead) or the concurrency cap is reached.
func (s *Server) startWorkspaceAgent(w http.ResponseWriter, r *http.Request) {
	ws := s.mgr.Workspace(r.PathValue("id"))
	if ws == nil {
		writeError(w, http.StatusNotFound, "unknown workspace")
		return
	}
	var req startAgentReq
	if !decodeJSON(w, r, &req) {
		return
	}
	launch, status, msg := s.resolveAgentLaunch(ws, r.PathValue("name"), req.Prompt)
	if msg != "" {
		writeError(w, status, msg)
		return
	}
	s.startOrWake(w, ws, req.CreatedBy, launch)
}

// resolveAgentLaunch turns a base name into a ready AgentLaunch for ws, or an
// HTTP status and message explaining why not.
func (s *Server) resolveAgentLaunch(ws *model.Workspace, name, prompt string) (manager.AgentLaunch, int, string) {
	if s.agents == nil || s.mgr.Harnesses == nil {
		return manager.AgentLaunch{}, http.StatusServiceUnavailable, "agents are not available on this daemon"
	}
	d, err := s.agents.Get(name)
	if errors.Is(err, agent.ErrNotFound) {
		return manager.AgentLaunch{}, http.StatusNotFound, "no such agent"
	}
	if err != nil {
		return manager.AgentLaunch{}, http.StatusServiceUnavailable, err.Error()
	}
	h, err := s.mgr.Harnesses.Resolve(d.Harness)
	if err != nil {
		return manager.AgentLaunch{}, http.StatusBadRequest, "agent harness: " + err.Error()
	}
	route, status, msg := s.agentRoute(d, h)
	if msg != "" {
		return manager.AgentLaunch{}, status, msg
	}
	dir, _, err := agent.Bootstrap(ws.RepoPath, d)
	if err != nil {
		return manager.AgentLaunch{}, http.StatusInternalServerError, "instance folder: " + err.Error()
	}
	if err := s.agents.WriteInstanceConfig(d, dir); err != nil {
		return manager.AgentLaunch{}, http.StatusInternalServerError, "instance config: " + err.Error()
	}
	port, status, msg := s.agentPort(ws.ID, d.Name, h)
	if msg != "" {
		return manager.AgentLaunch{}, status, msg
	}
	l := agent.LaunchCommand(d, h, s.agents.Dir(d.Name), ws.RepoPath, prompt, port)
	return manager.AgentLaunch{Name: d.Name, Version: d.Version, Harness: h, Persist: l.Persist, Deliver: l.Deliver, CWD: dir, RouteAccount: route, Prompt: prompt, Port: port}, 0, ""
}

// agentPort is the opencode server port for an instance: the one its pane
// was started with before (read back from the persisted command, so a wake
// keeps the address), else a fresh free port. 0 for other harnesses.
func (s *Server) agentPort(wsID, name string, h harness.Harness) (int, int, string) {
	if h.Name != "opencode" {
		return 0, 0, ""
	}
	if p := s.mgr.AgentPane(wsID, name); p != nil {
		if port := agent.OpencodePort(p.StartupCommand); port != 0 {
			return port, 0, ""
		}
	}
	port, err := agent.FreePort()
	if err != nil {
		return 0, http.StatusInternalServerError, "no free port for the opencode server: " + err.Error()
	}
	return port, 0, ""
}

// pushPromptLater delivers an opencode instance's first message through its
// server once it is up. Runs off the request: the pane is already live and
// the human sees the TUI come up; a push failure is logged, not a 5xx.
func (s *Server) pushPromptLater(l manager.AgentLaunch) {
	if l.Port == 0 || l.Prompt == "" {
		return
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
		defer cancel()
		if err := agent.PushPrompt(ctx, l.Port, l.Prompt); err != nil {
			log.Printf("agent %s: first prompt not delivered: %v", l.Name, err)
		}
	}()
}

// agentRoute picks the instance's llm account: the base's pin when it names
// one (and the harness may use that kind), else the harness's own pairing.
func (s *Server) agentRoute(d agent.Definition, h harness.Harness) (string, int, string) {
	if d.Account == "" {
		route, err := s.llmRouteForHarness(h)
		if err != nil {
			return "", http.StatusConflict, err.Error()
		}
		return route, 0, ""
	}
	if s.llm == nil {
		return "", http.StatusServiceUnavailable, "the agent pins an llm account but llm routing is not available on this daemon"
	}
	kind, err := s.llm.KindOf(d.Account)
	if err != nil {
		return "", http.StatusBadRequest, "agent account: " + err.Error()
	}
	if !kindAllowed(h.AccountKinds, kind) {
		return "", http.StatusBadRequest, "agent account " + d.Account + " is kind " + kind + ", which the " + h.Name + " harness cannot use"
	}
	return d.Account, 0, ""
}

// startOrWake spawns a new instance pane, or restarts the asleep one.
func (s *Server) startOrWake(w http.ResponseWriter, ws *model.Workspace, createdBy string, l manager.AgentLaunch) {
	if p := s.mgr.AgentPane(ws.ID, l.Name); p != nil {
		if !s.mgr.PaneAtShell(p.ID) {
			writeError(w, http.StatusConflict, "agent "+l.Name+" is running in this workspace — type into its pane")
			return
		}
		if msg := s.agentCapMessage(); msg != "" {
			writeError(w, http.StatusConflict, msg)
			return
		}
		if err := s.mgr.StartAgentInPane(p.ID, l); err != nil {
			writeError(w, http.StatusConflict, err.Error())
			return
		}
		s.pushPromptLater(l)
		writeJSON(w, http.StatusOK, s.mgr.AgentPane(ws.ID, l.Name))
		return
	}
	if msg := s.agentCapMessage(); msg != "" {
		writeError(w, http.StatusConflict, msg)
		return
	}
	p, err := s.mgr.SpawnAgentPane(ws.ID, createdBy, l)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	s.pushPromptLater(l)
	writeJSON(w, http.StatusCreated, p)
}

// agentCapMessage is "" while another agent may start.
func (s *Server) agentCapMessage() string {
	if s.agentsMax > 0 && s.mgr.RunningAgents() >= s.agentsMax {
		return "agent concurrency cap reached (" + strconv.Itoa(s.agentsMax) + " running) — wait for one to go idle, or raise -agents-max"
	}
	return ""
}

// stopWorkspaceAgent: DELETE /v1/workspaces/{id}/agents/{name} closes the
// instance's pane. The instance folder in the repo stays: it is the
// project's own instructions, memory and log for that agent.
func (s *Server) stopWorkspaceAgent(w http.ResponseWriter, r *http.Request) {
	ws := s.mgr.Workspace(r.PathValue("id"))
	if ws == nil {
		writeError(w, http.StatusNotFound, "unknown workspace")
		return
	}
	p := s.mgr.AgentPane(ws.ID, r.PathValue("name"))
	if p == nil {
		writeError(w, http.StatusNotFound, "agent not added to this workspace")
		return
	}
	if err := s.mgr.KillPane(ws.ID, p.ID); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
