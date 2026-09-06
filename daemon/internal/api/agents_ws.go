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

// agentInstance is one base agent as seen from one shared window: the base's
// identity plus whether it has been added there and what it is doing.
type agentInstance struct {
	Name        string `json:"name"`
	Icon        string `json:"icon"`
	Description string `json:"description"`
	Version     string `json:"version"`
	// State is "absent" (never added to this window), "asleep" (its session
	// sits at a shell or is archived; a message starts it) or "running".
	State string `json:"state"`
	// Workspace is the agent's own session in the window, Pane its agent
	// pane — what a lens opens to watch it.
	Workspace string `json:"workspace,omitempty"`
	Pane      string `json:"pane,omitempty"`
	// PaneVersion is the base version the instance last started with; Drift
	// says the base has moved since — a restart picks the new one up.
	PaneVersion string `json:"paneVersion,omitempty"`
	Drift       bool   `json:"drift,omitempty"`
}

// listWindowAgents: GET /v1/windows/{id}/agents.
func (s *Server) listWindowAgents(w http.ResponseWriter, r *http.Request) {
	win, ok := s.windowByID(r.PathValue("id"))
	if !ok {
		writeError(w, http.StatusNotFound, "unknown window")
		return
	}
	if !s.agentsReady(w) {
		return
	}
	out, err := s.windowInstances(win)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error()) // names the broken base; 503 is "no agents support"
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"agents": out})
}

// windowInstances is every base as seen from win.
func (s *Server) windowInstances(win manager.WindowInfo) ([]agentInstance, error) {
	defs, err := s.agents.List()
	if err != nil {
		return nil, err
	}
	out := make([]agentInstance, 0, len(defs))
	for _, d := range defs {
		out = append(out, s.instanceOf(win, d))
	}
	return out, nil
}

// windowByID finds a shared window; windowByName the same case-insensitively
// (the bus knows windows by name, lenses by id).
func (s *Server) windowByID(id string) (manager.WindowInfo, bool) {
	for _, win := range s.mgr.Windows() {
		if win.ID == id {
			return win, true
		}
	}
	return manager.WindowInfo{}, false
}

func (s *Server) windowByName(name string) (manager.WindowInfo, bool) {
	id, ok := s.mgr.WindowByName(name)
	if !ok {
		return manager.WindowInfo{}, false
	}
	return s.windowByID(id)
}

// agentWorkspace is the session in win that IS base name's instance, live or
// archived, or nil when the agent was never added there. Agents run on this
// host: a member this daemon has never heard of cannot be one.
func (s *Server) agentWorkspace(win manager.WindowInfo, name string) *model.Workspace {
	for _, id := range win.WorkspaceIDs {
		if ws := s.mgr.Workspace(id); ws != nil && ws.Agent == name {
			return ws
		}
	}
	return nil
}

// windowRepos lists the project folders of win's ordinary sessions on this
// host — what the agent gets read access to (--add-dir).
func (s *Server) windowRepos(win manager.WindowInfo) []string {
	var dirs []string
	for _, id := range win.WorkspaceIDs {
		if ws := s.mgr.Workspace(id); ws != nil && ws.Agent == "" {
			dirs = append(dirs, ws.RepoPath)
		}
	}
	return dirs
}

func (s *Server) instanceOf(win manager.WindowInfo, d agent.Definition) agentInstance {
	inst := agentInstance{Name: d.Name, Icon: d.Icon, Description: d.Description, Version: d.Version, State: "absent"}
	ws := s.agentWorkspace(win, d.Name)
	if ws == nil {
		return inst
	}
	inst.Workspace, inst.State = ws.ID, "asleep"
	p := s.mgr.AgentPane(ws.ID, d.Name)
	if p == nil {
		return inst
	}
	inst.Pane, inst.PaneVersion, inst.Drift = p.ID, p.AgentVersion, agent.Drifted(p.AgentVersion, d.Version)
	if ws.Status == model.StatusLive && !s.mgr.PaneAtShell(p.ID) {
		inst.State = "running"
	}
	return inst
}

type startAgentReq struct {
	Prompt    string `json:"prompt"`
	CreatedBy string `json:"createdBy"`
}

// startWindowAgentRoute: POST /v1/windows/{id}/agents/{name} adds the base
// to the window as its own session (instance folder bootstrapped once,
// opencode config regenerated) and starts it with the prompt as its first
// message, or wakes an asleep instance the same way. 409 when the instance
// is already running (type into its pane instead) or the concurrency cap is
// reached.
func (s *Server) startWindowAgentRoute(w http.ResponseWriter, r *http.Request) {
	win, ok := s.windowByID(r.PathValue("id"))
	if !ok {
		writeError(w, http.StatusNotFound, "unknown window")
		return
	}
	var req startAgentReq
	if !decodeJSON(w, r, &req) {
		return
	}
	out := s.startWindowAgent(win, r.PathValue("name"), req.Prompt, req.CreatedBy)
	if out.msg != "" {
		writeError(w, out.status, out.msg)
		return
	}
	writeJSON(w, out.status, out.pane)
}

// startWindowAgent is the ONE add-or-wake flow for a window: an instance
// already there is woken through startAgent, a missing one is created as a
// new session in the window. The HTTP route and the bus are adapters over it.
func (s *Server) startWindowAgent(win manager.WindowInfo, name, prompt, createdBy string) startOutcome {
	if s.agents == nil {
		return startOutcome{status: http.StatusServiceUnavailable, msg: agentsUnavailable}
	}
	if ws := s.agentWorkspace(win, name); ws != nil {
		return s.startAgent(ws.ID, name, prompt, createdBy)
	}
	d, err := s.agents.Get(name)
	if errors.Is(err, agent.ErrNotFound) {
		return startOutcome{status: http.StatusNotFound, msg: "no such agent"}
	}
	if err != nil {
		return startOutcome{status: http.StatusServiceUnavailable, msg: err.Error()}
	}
	if msg := s.agentCapMessage(); msg != "" {
		return startOutcome{status: http.StatusConflict, msg: msg, err: manager.ErrAgentCap}
	}
	l, status, msg := s.resolveAgentLaunch(s.agents.InstanceDir(win.ID, win.Name, name), s.windowRepos(win), "", name, prompt)
	if msg != "" {
		return startOutcome{status: status, msg: msg}
	}
	ws, err := s.mgr.CreateAgentWorkspace(agentSessionName(d), createdBy, win.Name, l)
	if err != nil {
		return startOutcome{status: agentErrStatus(err), msg: err.Error(), err: err}
	}
	if err := s.mgr.SeedWindowMembership(ws.ID, win.Name); err != nil {
		// Without the membership row the window never finds this session
		// again (it walks members), and the next add would make a second one
		// on the same folder: undo, and say so.
		if kerr := s.mgr.KillWorkspace(ws.ID); kerr != nil {
			log.Printf("agent %s: session %s left behind after a membership failure: %v", name, ws.ID, kerr)
		}
		return startOutcome{status: http.StatusInternalServerError, msg: "window membership: " + err.Error(), err: err}
	}
	s.pushPromptLater(ws.Panes[0].ID, l)
	return startOutcome{pane: ws.Panes[0], status: http.StatusCreated}
}

// agentSessionName is the session row's title: the base's icon and name.
func agentSessionName(d agent.Definition) string {
	if d.Icon == "" {
		return d.Name
	}
	return d.Icon + " " + d.Name
}

// resolveAgentLaunch turns a base name into a ready AgentLaunch for the
// instance folder dir (written here) with dirs as the project folders, or an
// HTTP status and message explaining why not. wsID is the instance's
// existing session when it has one (its opencode port is reused), "" for a
// fresh add.
func (s *Server) resolveAgentLaunch(dir string, dirs []string, wsID, name, prompt string) (manager.AgentLaunch, int, string) {
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
	if _, err := agent.Bootstrap(dir, d); err != nil {
		return manager.AgentLaunch{}, http.StatusInternalServerError, "instance folder: " + err.Error()
	}
	if err := s.agents.WriteInstanceConfig(d, dir); err != nil {
		return manager.AgentLaunch{}, http.StatusInternalServerError, "instance config: " + err.Error()
	}
	port, status, msg := s.agentPort(wsID, d.Name, h)
	if msg != "" {
		return manager.AgentLaunch{}, status, msg
	}
	l := agent.LaunchCommand(d, h, s.agents.Dir(d.Name), dirs, prompt, port)
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
func (s *Server) pushPromptLater(paneID string, l manager.AgentLaunch) {
	if l.Port == 0 || l.Prompt == "" {
		return
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
		defer cancel()
		// Under the pane's push lock: a bus message retried during this start
		// must queue behind the first prompt, not interleave with it.
		err := s.mgr.PushLocked(paneID, func() error { return agent.PushPrompt(ctx, l.Port, l.Prompt) })
		if err != nil {
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

// startAgent is the ONE wake flow for an existing agent session wsID: cap
// and "already running" first (before any side effect), then launch
// resolution (which refreshes the instance folder's generated config), then
// the start into its pane, then the first prompt. An archived session is
// revived instead, which replays its persisted launch. Returns the pane, the
// HTTP status a transport would answer with (200 woken) and the refusal
// message, "" when it went through. The window route, the bus and the
// lifecycle loop are all adapters over this.
func (s *Server) startAgent(wsID, name, prompt, createdBy string) startOutcome {
	ws := s.mgr.Workspace(wsID)
	if ws == nil || ws.Agent != name {
		return startOutcome{status: http.StatusNotFound, msg: "no session for agent " + name + " here"}
	}
	existing := s.mgr.AgentPane(wsID, name)
	if existing == nil {
		return startOutcome{status: http.StatusConflict, msg: "agent " + name + "'s session lost its agent pane — remove the session and add the agent again"}
	}
	if ws.Status == model.StatusLive && !s.mgr.PaneAtShell(existing.ID) {
		return startOutcome{pane: existing, status: http.StatusConflict, msg: "agent " + name + " is running in this window — type into its pane", err: manager.ErrAgentRunning}
	}
	if msg := s.agentCapMessage(); msg != "" {
		return startOutcome{status: http.StatusConflict, msg: msg, err: manager.ErrAgentCap}
	}
	l, status, msg := s.resolveAgentLaunch(ws.RepoPath, s.windowReposOfPane(existing.ID), wsID, name, prompt)
	if msg != "" {
		return startOutcome{status: status, msg: msg}
	}
	if out := s.launchIntoAgentSession(ws, existing.ID, l); out.msg != "" {
		return out
	}
	s.pushPromptLater(existing.ID, l)
	return startOutcome{pane: s.mgr.AgentPane(wsID, name), status: http.StatusOK}
}

// launchIntoAgentSession types the launch into a live agent session's pane,
// or revives an archived session with it (the replay carries the prompt).
// The outcome's msg is "" when it went through.
func (s *Server) launchIntoAgentSession(ws *model.Workspace, paneID string, l manager.AgentLaunch) startOutcome {
	if ws.Status != model.StatusLive {
		if _, err := s.mgr.ReviveAgentWorkspace(ws.ID, l); err != nil {
			return startOutcome{status: agentErrStatus(err), msg: "revive agent session: " + err.Error(), err: err}
		}
		return startOutcome{}
	}
	if err := s.mgr.StartAgentInPane(paneID, l); err != nil {
		return startOutcome{status: agentErrStatus(err), msg: err.Error(), err: err}
	}
	return startOutcome{}
}

// windowReposOfPane is windowRepos for the window the pane's session sits in
// (its RESOLVED window, not the legacy column); nil when it has none.
func (s *Server) windowReposOfPane(paneID string) []string {
	group, ok := s.mgr.GroupForPane(paneID)
	if !ok || group == "" {
		return nil
	}
	win, ok := s.windowByName(group)
	if !ok {
		return nil
	}
	return s.windowRepos(win)
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

// sleepWindowAgent: DELETE /v1/windows/{id}/agents/{name} puts the instance
// to sleep the way idle exit does: the session and its folder stay (the
// project's own instructions, memory and log for that agent), the pane drops
// to its shell. Removing the session is the session's own Remove.
func (s *Server) sleepWindowAgent(w http.ResponseWriter, r *http.Request) {
	win, ok := s.windowByID(r.PathValue("id"))
	if !ok {
		writeError(w, http.StatusNotFound, "unknown window")
		return
	}
	ws := s.agentWorkspace(win, r.PathValue("name"))
	if ws == nil {
		writeError(w, http.StatusNotFound, "agent not added to this window")
		return
	}
	if p := s.mgr.AgentPane(ws.ID, ws.Agent); p != nil && ws.Status == model.StatusLive {
		if err := s.mgr.SleepAgent(p.ID); err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
	}
	w.WriteHeader(http.StatusNoContent)
}
