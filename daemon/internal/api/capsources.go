package api

import (
	"errors"
	"fmt"
	"log"
	"net/http"

	"ccmux.dev/ccmuxd/internal/agent"
	"ccmux.dev/ccmuxd/internal/harness"
)

// The catalog's git sources and pins (agent/capsources.go, cappin.go).
// Routes are listed in capabilities.go.

func (s *Server) listCapSources(w http.ResponseWriter, r *http.Request) {
	if !s.agentsReady(w) {
		return
	}
	list, err := s.agents.Sources()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"sources": list})
}

// addCapSource records a source and fetches it at once; a source whose
// first fetch fails is not kept, so a typo in its url leaves nothing.
func (s *Server) addCapSource(w http.ResponseWriter, r *http.Request) {
	if !s.agentsReady(w) {
		return
	}
	var src agent.CapSource
	if !decodeJSON(w, r, &src) {
		return
	}
	src, err := s.agents.AddSource(src)
	switch {
	case errors.Is(err, agent.ErrSourceExists):
		writeError(w, http.StatusConflict, err.Error())
		return
	case err != nil:
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	res, err := s.agents.UpdateSource(r.Context(), src.ID)
	if err != nil {
		if rerr := s.agents.RemoveSource(src.ID); rerr != nil {
			log.Printf("capabilities: source %s failed its first fetch, and removing it again failed (%v); remove it by hand", src.ID, rerr)
			err = fmt.Errorf("%w; removing the source again failed too, remove %s by hand: %v", err, src.ID, rerr)
		}
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, res)
}

func (s *Server) deleteCapSource(w http.ResponseWriter, r *http.Request) {
	if !s.agentsReady(w) {
		return
	}
	err := s.agents.RemoveSource(r.PathValue("id"))
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

func (s *Server) updateCapSource(w http.ResponseWriter, r *http.Request) {
	if !s.agentsReady(w) {
		return
	}
	res, err := s.agents.UpdateSource(r.Context(), r.PathValue("id"))
	switch {
	case errors.Is(err, agent.ErrNotFound):
		writeError(w, http.StatusNotFound, "not found")
		return
	case err != nil:
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, res)
}

func (s *Server) pinCapability(w http.ResponseWriter, r *http.Request) {
	if !s.agentsReady(w) {
		return
	}
	var body struct {
		Pinned bool `json:"pinned"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	c, err := s.agents.PinCapability(r.Context(), r.PathValue("id"), body.Pinned)
	if err != nil {
		s.storeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, c)
}

// probeSession runs a probe (agent/capprobe.go) with this host's claude.
func (s *Server) probeSession(r *http.Request) (agent.ProbeResult, error) {
	claude, err := harness.LookPath("claude")
	if err != nil {
		return agent.ProbeResult{}, err
	}
	return s.agents.RunProbe(r.Context(), claude)
}
