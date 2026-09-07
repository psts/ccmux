package api

import (
	"context"
	"errors"
	"io"
	"log"
	"net/http"
	"time"

	"ccmux.dev/ccmuxd/internal/agent"
)

// The base's parts beyond agent.json: skills (from a git folder URL or
// dropped files), MCP servers (from a pasted snippet), and the instances a
// base is deployed as, with a restart that makes a changed base take
// effect. Every change bumps the base version so instances show drift.

func (s *Server) agentNamed(w http.ResponseWriter, r *http.Request) (string, bool) {
	if !s.agentsReady(w) {
		return "", false
	}
	name := r.PathValue("name")
	if _, err := s.agents.Get(name); err != nil {
		writeError(w, http.StatusNotFound, "no such agent")
		return "", false
	}
	return name, true
}

func (s *Server) storeError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, agent.ErrNotFound):
		writeError(w, http.StatusNotFound, "not found")
	default:
		writeError(w, http.StatusBadRequest, err.Error())
	}
}

// listAgentSkills: GET /v1/agents/{name}/skills
func (s *Server) listAgentSkills(w http.ResponseWriter, r *http.Request) {
	name, ok := s.agentNamed(w, r)
	if !ok {
		return
	}
	list, err := s.agents.Skills(name)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"skills": list})
}

// addAgentSkill: POST /v1/agents/{name}/skills with {"url"} installs from a
// git folder; with {"name","files":{path: text}} installs dropped files.
func (s *Server) addAgentSkill(w http.ResponseWriter, r *http.Request) {
	name, ok := s.agentNamed(w, r)
	if !ok {
		return
	}
	var req struct {
		URL   string            `json:"url"`
		Name  string            `json:"name"`
		Files map[string]string `json:"files"`
	}
	r.Body = http.MaxBytesReader(w, r.Body, 8<<20)
	if !decodeJSON(w, r, &req) {
		return
	}
	var sk agent.Skill
	var err error
	switch {
	case req.URL != "":
		var src agent.GitSource
		if src, err = agent.ParseSkillURL(req.URL); err == nil {
			ctx, cancel := context.WithTimeout(r.Context(), 2*time.Minute)
			defer cancel()
			sk, err = s.agents.InstallSkillFromGit(ctx, name, src, req.URL)
		}
	case len(req.Files) > 0:
		sk, err = s.agents.PutSkillFiles(name, req.Name, req.Files)
	default:
		err = errors.New("give a url, or files")
	}
	if err != nil {
		s.storeError(w, err)
		return
	}
	log.Printf("agent %s: skill %s installed", name, sk.Name)
	writeJSON(w, http.StatusOK, sk)
}

// agentSkillText: GET /v1/agents/{name}/skills/{skill} → SKILL.md as text.
func (s *Server) agentSkillText(w http.ResponseWriter, r *http.Request) {
	name, ok := s.agentNamed(w, r)
	if !ok {
		return
	}
	text, err := s.agents.SkillText(name, r.PathValue("skill"))
	if err != nil {
		s.storeError(w, err)
		return
	}
	w.Header().Set("Content-Type", "text/markdown; charset=utf-8")
	io.WriteString(w, text)
}

// updateAgentSkill: POST /v1/agents/{name}/skills/{skill}/update refetches
// from the recorded source.
func (s *Server) updateAgentSkill(w http.ResponseWriter, r *http.Request) {
	name, ok := s.agentNamed(w, r)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Minute)
	defer cancel()
	sk, err := s.agents.UpdateSkill(ctx, name, r.PathValue("skill"))
	if err != nil {
		s.storeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, sk)
}

// deleteAgentSkill: DELETE /v1/agents/{name}/skills/{skill}
func (s *Server) deleteAgentSkill(w http.ResponseWriter, r *http.Request) {
	name, ok := s.agentNamed(w, r)
	if !ok {
		return
	}
	if err := s.agents.DeleteSkill(name, r.PathValue("skill")); err != nil {
		s.storeError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// listAgentMCP: GET /v1/agents/{name}/mcp
func (s *Server) listAgentMCP(w http.ResponseWriter, r *http.Request) {
	name, ok := s.agentNamed(w, r)
	if !ok {
		return
	}
	list, err := s.agents.MCPServers(name)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"servers": list})
}

// addAgentMCP: POST /v1/agents/{name}/mcp with the pasted snippet as body.
func (s *Server) addAgentMCP(w http.ResponseWriter, r *http.Request) {
	name, ok := s.agentNamed(w, r)
	if !ok {
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 1<<20))
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	names, err := s.agents.AddMCP(name, body)
	if err != nil {
		s.storeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"added": names})
}

// deleteAgentMCP: DELETE /v1/agents/{name}/mcp/{server}
func (s *Server) deleteAgentMCP(w http.ResponseWriter, r *http.Request) {
	name, ok := s.agentNamed(w, r)
	if !ok {
		return
	}
	if err := s.agents.RemoveMCP(name, r.PathValue("server")); err != nil {
		s.storeError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// agentDeployment is one window the base is added to.
type agentDeployment struct {
	Window   string `json:"window"`
	WindowID string `json:"windowId"`
	agentInstance
}

// listAgentInstances: GET /v1/agents/{name}/instances → every window the
// base is added to, with its state there and whether it runs an older
// version (drift).
func (s *Server) listAgentInstances(w http.ResponseWriter, r *http.Request) {
	name, ok := s.agentNamed(w, r)
	if !ok {
		return
	}
	list, status, msg := s.deployments(name)
	if msg != "" {
		writeError(w, status, msg)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"instances": list})
}

func (s *Server) deployments(name string) ([]agentDeployment, int, string) {
	d, err := s.agents.Get(name)
	if err != nil {
		return nil, http.StatusNotFound, "no such agent"
	}
	wins, err := s.mgr.WindowsListStrict()
	if err != nil {
		return nil, http.StatusServiceUnavailable, "windows: " + err.Error()
	}
	out := []agentDeployment{}
	for _, win := range wins {
		inst := s.instanceOf(win, d)
		if inst.State == "absent" {
			continue
		}
		out = append(out, agentDeployment{Window: win.Name, WindowID: win.ID, agentInstance: inst})
	}
	return out, 0, ""
}

// restartAgentInstances: POST /v1/agents/{name}/instances/restart puts every
// RUNNING instance to sleep and starts it again from the current base, off
// the request; asleep ones pick the base up on their own next start.
// Answers with how many it is restarting.
func (s *Server) restartAgentInstances(w http.ResponseWriter, r *http.Request) {
	name, ok := s.agentNamed(w, r)
	if !ok {
		return
	}
	list, status, msg := s.deployments(name)
	if msg != "" {
		writeError(w, status, msg)
		return
	}
	n := 0
	for _, inst := range list {
		if inst.State != "running" {
			continue
		}
		n++
		go s.restartInstance(name, inst)
	}
	writeJSON(w, http.StatusAccepted, map[string]any{"restarting": n})
}

func (s *Server) restartInstance(name string, inst agentDeployment) {
	if err := s.mgr.SleepAgent(inst.Pane); err != nil {
		log.Printf("agent %s in %s: restart: sleep: %v", name, inst.Window, err)
		return
	}
	deadline := time.Now().Add(30 * time.Second)
	for !s.mgr.PaneAtShell(inst.Pane) {
		if time.Now().After(deadline) {
			log.Printf("agent %s in %s: restart: did not go to sleep within 30s (a permission dialog up?); left as is", name, inst.Window)
			return
		}
		time.Sleep(500 * time.Millisecond)
	}
	if out := s.startAgent(inst.Workspace, name, "", nil); out.msg != "" {
		log.Printf("agent %s in %s: restart: start: %s", name, inst.Window, out.msg)
	}
}
