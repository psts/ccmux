package api

import (
	"errors"
	"net/http"
	"sort"

	"ccmux.dev/ccmuxd/internal/agent"
)

// The capability catalog (agent/capabilities.go): what an isolated claude
// agent may be given besides its own base. Both lenses list it in the
// agent editor as checkboxes and add to it from there.
//
//	GET    /v1/capabilities          {capabilities, installed}
//	PUT    /v1/capabilities/{id}     add or replace one entry
//	DELETE /v1/capabilities/{id}     409 while a base still names it
//	POST   /v1/capabilities/import   seed from the host's Claude setup

// capabilityView is an entry as the lenses read it: the entry plus the
// skills a plugin brings (<plugin>:<skill>), for the editor's skill list.
type capabilityView struct {
	agent.Capability
	Skills []string `json:"skills"`
}

func (s *Server) listCapabilities(w http.ResponseWriter, r *http.Request) {
	if !s.agentsReady(w) {
		return
	}
	list, err := s.agents.Capabilities()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	installed, err := s.installedPlugins()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	views := make([]capabilityView, 0, len(list))
	for _, c := range list {
		views = append(views, capabilityView{Capability: c, Skills: capabilitySkills(c, installed)})
	}
	ids := make([]string, 0, len(installed))
	for id := range installed {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	writeJSON(w, http.StatusOK, map[string]any{"capabilities": views, "installed": ids})
}

// capabilitySkills is what a plugin entry brings; none for a server, or
// for a plugin this host cannot find (its start says so, not the list).
func capabilitySkills(c agent.Capability, installed map[string]string) []string {
	dir := c.Path
	if c.Kind != agent.CapPlugin {
		return []string{}
	}
	if dir == "" {
		dir = installed[c.Plugin]
	}
	names, err := agent.PluginSkills(dir)
	if err != nil || names == nil {
		return []string{}
	}
	return names
}

func (s *Server) installedPlugins() (map[string]string, error) {
	claudeDir, err := agent.ClaudeConfigDir()
	if err != nil {
		return nil, err
	}
	return agent.ReadInstalledPlugins(agent.InstalledPluginsPath(claudeDir))
}

func (s *Server) putCapability(w http.ResponseWriter, r *http.Request) {
	if !s.agentsReady(w) {
		return
	}
	var c agent.Capability
	if !decodeJSON(w, r, &c) {
		return
	}
	c.ID = r.PathValue("id")
	saved, err := s.agents.SaveCapability(c)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, saved)
}

func (s *Server) deleteCapability(w http.ResponseWriter, r *http.Request) {
	if !s.agentsReady(w) {
		return
	}
	err := s.agents.RemoveCapability(r.PathValue("id"))
	switch {
	case errors.Is(err, agent.ErrCapabilityInUse):
		writeError(w, http.StatusConflict, err.Error())
		return
	case err != nil:
		s.storeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) importCapabilities(w http.ResponseWriter, r *http.Request) {
	if !s.agentsReady(w) {
		return
	}
	claudeDir, err := agent.ClaudeConfigDir()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	res, err := s.agents.ImportCapabilities(claudeDir)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, res)
}
