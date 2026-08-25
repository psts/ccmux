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
	route, err := s.llmRouteForHarness(h.Name)
	if err != nil {
		writeError(w, http.StatusConflict, err.Error())
		return
	}
	if route == "" {
		s.clearForeignPaneRoute(paneID)
	}
	if err := s.mgr.StartHarnessInPane(paneID, h, route); err != nil {
		code := http.StatusInternalServerError
		if errors.Is(err, manager.ErrPaneBusy) {
			code = http.StatusConflict
		}
		writeError(w, code, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"pane": paneID, "harness": h.Name})
}

// llmRouteForHarness resolves the llm account a harness start must route its
// pane to ("" = leave routing alone). Keyed by harness NAME, not a stored
// field, so a user override of the codex entry (different flags, icon) keeps
// its pairing: codex can only talk to a ChatGPT-backed account, so its pane
// is pointed at the first codex-kind account before the command types.
func (s *Server) llmRouteForHarness(name string) (string, error) {
	if name != "codex" {
		return "", nil
	}
	if s.llm == nil {
		return "", errors.New("the codex harness needs the llm proxy, which is not available on this daemon")
	}
	return s.llm.AccountNameForKind("codex")
}

// clearForeignPaneRoute drops a pane's llm override when it names a
// codex-kind account and a NON-codex harness is about to start there:
// Anthropic-dialect traffic into the ChatGPT backend fails on every request
// (and the proxy refuses it — see llmproxy.Handler). Best-effort: a read
// failure leaves the route for the proxy's own loud errors to surface.
func (s *Server) clearForeignPaneRoute(paneID string) {
	if s.llm == nil {
		return
	}
	routes, err := s.llm.PaneRoutes()
	if err != nil {
		return
	}
	name, ok := routes[paneID]
	if !ok {
		return
	}
	if kind, err := s.llm.KindOf(name); err == nil && kind == "codex" {
		_ = s.llm.SetPaneRoute(paneID, "")
	}
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
