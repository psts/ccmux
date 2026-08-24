package api

import (
	"errors"
	"net/http"

	"ccmux.dev/ccmuxd/internal/manager"
)

// startPaneHarness starts a named harness INSIDE an existing pane — the
// picker's "start it here" on a shell pane, as opposed to spawnPane's new
// tab. Scoped, so a hub-fronted lens lands on the pane's owning host.
func (s *Server) startPaneHarness(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Harness string `json:"harness"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	paneID := r.PathValue("id")
	if s.mgr.WorkspaceForPane(paneID) == "" {
		writeError(w, http.StatusNotFound, "unknown pane")
		return
	}
	if s.mgr.Harnesses == nil {
		writeError(w, http.StatusBadRequest, "harnesses are not available on this daemon")
		return
	}
	h, err := s.mgr.Harnesses.Resolve(req.Harness)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := s.mgr.StartHarnessInPane(paneID, h); err != nil {
		code := http.StatusInternalServerError
		if errors.Is(err, manager.ErrPaneBusy) {
			code = http.StatusConflict
		}
		writeError(w, code, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"pane": paneID, "harness": h.Name})
}

// resolvedHarnessFor names the harness a folder's resolved startup command
// would run — what a picker preselects. An exact match against a configured
// harness command wins; otherwise the claude-command guess answers.
func (s *Server) resolvedHarnessFor(cmd string) string {
	if hs, err := s.mgr.Harnesses.List(); err == nil {
		for _, h := range hs {
			if h.Command == cmd {
				return h.Name
			}
		}
	}
	return s.mgr.GuessHarness(cmd)
}
