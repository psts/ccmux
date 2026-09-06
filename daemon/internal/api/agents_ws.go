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
	if !s.agentsReady(w) {
		return
	}
	defs, err := s.agents.List()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error()) // names the broken base; 503 is "no agents support"
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
	inst.Pane, inst.PaneVersion, inst.Drift = p.ID, p.AgentVersion, agent.Drifted(p.AgentVersion, d.Version)
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
	out := s.startAgent(ws.ID, r.PathValue("name"), req.Prompt, req.CreatedBy)
	if out.msg != "" {
		writeError(w, out.status, out.msg)
		return
	}
	writeJSON(w, out.status, out.pane)
}

// resolveAgentLaunch turns a base name into a ready AgentLaunch for ws, or an
// HTTP status and message explaining why not.
func (s *Server) resolveAgentLaunch(ws *model.Workspace, name, prompt string) (manager.AgentLaunch, int, string) {
	if s.agents == nil || s.mgr.Harnesses == nil {
		return manager.AgentLaunch{}, http.StatusServiceUnavailable, agentsUnavailable
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

// startAgent is the ONE start-or-wake flow: cap and "already running" first
// (before any side effect), then launch resolution (which writes the
// instance folder), then spawn or wake, then the first prompt. Returns the
// pane, the HTTP status a transport would answer with (201 added, 200 woken)
// and the refusal message, "" when it went through. The HTTP route, the bus
// and the lifecycle loop are all adapters over this.
func (s *Server) startAgent(wsID, name, prompt, createdBy string) startOutcome {
	ws := s.mgr.Workspace(wsID)
	if ws == nil {
		return startOutcome{status: http.StatusNotFound, msg: "unknown workspace"}
	}
	existing := s.mgr.AgentPane(wsID, name)
	if existing != nil && !s.mgr.PaneAtShell(existing.ID) {
		return startOutcome{pane: existing, status: http.StatusConflict, msg: "agent " + name + " is running in this workspace — type into its pane", err: manager.ErrAgentRunning}
	}
	if msg := s.agentCapMessage(); msg != "" {
		return startOutcome{status: http.StatusConflict, msg: msg, err: manager.ErrAgentCap}
	}
	l, status, msg := s.resolveAgentLaunch(ws, name, prompt)
	if msg != "" {
		return startOutcome{status: status, msg: msg}
	}
	if existing != nil {
		if err := s.mgr.StartAgentInPane(existing.ID, l); err != nil {
			return startOutcome{status: agentErrStatus(err), msg: err.Error(), err: err}
		}
		s.pushPromptLater(l)
		return startOutcome{pane: s.mgr.AgentPane(wsID, name), status: http.StatusOK}
	}
	p, err := s.mgr.SpawnAgentPane(wsID, createdBy, l)
	if err != nil {
		return startOutcome{status: agentErrStatus(err), msg: err.Error(), err: err}
	}
	s.pushPromptLater(l)
	return startOutcome{pane: p, status: http.StatusCreated}
}

// startOutcome is what startAgent hands its adapters: the pane, the HTTP
// status a transport answers with, the refusal message ("" = went through),
// and the manager's sentinel when one caused the refusal — so a non-HTTP
// caller can tell "already running" from a real failure without parsing text.
type startOutcome struct {
	pane   *model.Pane
	status int
	msg    string
	err    error
}

// agentErrStatus maps the manager's refusals to HTTP: the cap and a duplicate
// instance are 409 (try again later / it is running), the rest 400.
func agentErrStatus(err error) int {
	if errors.Is(err, manager.ErrAgentRunning) || errors.Is(err, manager.ErrAgentCap) || errors.Is(err, manager.ErrPaneBusy) {
		return http.StatusConflict
	}
	return http.StatusBadRequest
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
