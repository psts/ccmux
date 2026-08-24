package api

import (
	"errors"
	"net/http"

	"ccmux.dev/ccmuxd/internal/llmproxy"
)

// Pane LLM route: the per-pane override behind the /llm/pane/… proxy. Both
// handlers run s.scoped, so a hub-fronted lens reads and writes the route on
// the host that owns the pane — where that pane's proxy resolves it.

func (s *Server) getPaneLLMRoute(w http.ResponseWriter, r *http.Request) {
	s.respondPaneLLMRoute(w, r.PathValue("id"))
}

func (s *Server) putPaneLLMRoute(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Route string `json:"route"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	paneID := r.PathValue("id")
	if s.mgr.WorkspaceForPane(paneID) == "" {
		writeError(w, http.StatusNotFound, "unknown pane")
		return
	}
	if err := s.llm.SetPaneRoute(paneID, req.Route); err != nil {
		code := http.StatusInternalServerError
		if errors.Is(err, llmproxy.ErrUnknownAccount) {
			code = http.StatusBadRequest
		}
		writeError(w, code, err.Error())
		return
	}
	s.respondPaneLLMRoute(w, paneID)
}

// respondPaneLLMRoute is the shared answer shape: the explicit override (""
// = following the global route), the account actually answering, and the
// names a picker can offer — from THIS host's accounts, which is what the
// pane's proxy resolves against.
func (s *Server) respondPaneLLMRoute(w http.ResponseWriter, paneID string) {
	if s.mgr.WorkspaceForPane(paneID) == "" {
		writeError(w, http.StatusNotFound, "unknown pane")
		return
	}
	explicit, effective, err := s.llm.PaneStatus(paneID)
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, err.Error())
		return
	}
	accs, _, err := s.llm.Snapshot()
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, err.Error())
		return
	}
	names := make([]string, 0, len(accs))
	for _, a := range accs {
		names = append(names, a.Name)
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"pane": paneID, "route": explicit, "effective": effective, "accounts": names,
	})
}
