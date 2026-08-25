package api

import (
	"errors"
	"fmt"
	"log"
	"net/http"
	"strings"

	"ccmux.dev/ccmuxd/internal/harness"
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
	route, err := s.llmRouteForHarness(h)
	if err != nil {
		writeError(w, http.StatusConflict, err.Error())
		return
	}
	if route == "" {
		s.clearForeignPaneRoute(paneID, h)
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
// pane to ("" = the pane's default routing already works). It reads the
// harness's resolved AccountKinds (registry defaults fill empty ones, so a
// user override of the codex command keeps codex's pairing): when the global
// route's account kind is not among them, the pane is pointed at the first
// account of an allowed kind before the command types.
func (s *Server) llmRouteForHarness(h harness.Harness) (string, error) {
	if len(h.AccountKinds) == 0 {
		return "", nil
	}
	if s.llm == nil {
		return "", fmt.Errorf("the %s harness needs the llm proxy, which is not available on this daemon", h.Name)
	}
	globalKind, err := s.globalRouteKind()
	if err != nil {
		return "", err
	}
	if kindAllowed(h.AccountKinds, globalKind) {
		return "", nil
	}
	for _, k := range h.AccountKinds {
		if name, err := s.llm.AccountNameForKind(k); err == nil {
			return name, nil
		}
	}
	return "", fmt.Errorf("the %s harness pairs with %s llm accounts — add one under settings, Accounts", h.Name, strings.Join(h.AccountKinds, " or "))
}

// globalRouteKind is the kind of the account the global route names —
// "anthropic" for the empty route's direct pass-through.
func (s *Server) globalRouteKind() (string, error) {
	route, err := s.llm.Route()
	if err != nil {
		return "", err
	}
	if route == "" {
		return "anthropic", nil
	}
	return s.llm.KindOf(route)
}

// clearForeignPaneRoute drops a pane's llm override when the harness about
// to start there cannot use it (a codex route left behind for a claude
// start, say) — otherwise every request fails on a stale decision.
// Best-effort: a read failure leaves the route for the proxy's own loud
// errors to surface.
func (s *Server) clearForeignPaneRoute(paneID string, h harness.Harness) {
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
	kind, err := s.llm.KindOf(name)
	if err != nil || kind == "" {
		return
	}
	if !kindAllowed(h.AccountKinds, kind) {
		if err := s.llm.SetPaneRoute(paneID, ""); err != nil {
			// The decision was made and the write failed: without this line,
			// the pane's every llm request fails on a cause nothing recorded.
			log.Printf("llm: pane %s: could not clear stale %s route for %s start: %v", paneID, kind, h.Name, err)
		}
	}
}
