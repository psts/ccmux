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
	if err := s.checkHarnessAccounts(h); err != nil {
		writeError(w, http.StatusConflict, err.Error())
		return
	}
	// An override the starting harness cannot use is cleared rather than left
	// to fail every request in the pane.
	s.clearForeignPaneRoute(paneID, h)
	if err := s.mgr.StartHarnessInPane(paneID, h, ""); err != nil {
		code := http.StatusInternalServerError
		if errors.Is(err, manager.ErrPaneBusy) {
			code = http.StatusConflict
		}
		writeError(w, code, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"pane": paneID, "harness": h.Name})
}

// checkHarnessAccounts refuses a harness start that has nothing to talk to,
// before the pane exists: starting codex with no ChatGPT account configured
// should say so, not start and then 502 on the first request.
//
// It no longer PINS a pane route the way it did when one global account
// decided for every pane. The proxy resolves a harness pane against that
// harness's own kinds and order per request (llmproxy.candidatesFor), so
// writing an override here would do worse than duplicate it: the override
// becomes the head of the order, so the first account of the first allowed
// KIND would silently outrank the order the user actually set.
func (s *Server) checkHarnessAccounts(h harness.Harness) error {
	if len(h.AccountKinds) == 0 {
		return nil
	}
	if s.llm == nil {
		return fmt.Errorf("the %s harness needs the llm proxy, which is not available on this daemon", h.Name)
	}
	// The default account counts. With nothing configured at all it is the
	// direct Anthropic pass-through, which an anthropic-dialect harness can
	// use perfectly well — demanding a configured account here would refuse
	// to start opencode on a daemon that needs no accounts to work.
	defaultKind, err := s.defaultRouteKind()
	if err != nil {
		return err
	}
	if kindAllowed(h.AccountKinds, defaultKind) {
		return nil
	}
	for _, k := range h.AccountKinds {
		if _, err := s.llm.AccountNameForKind(k); err == nil {
			return nil
		}
	}
	return fmt.Errorf("the %s harness pairs with %s llm accounts — add one under settings, Accounts", h.Name, strings.Join(h.AccountKinds, " or "))
}

// defaultRouteKind is the kind of the account the default route names —
// "anthropic" for the empty route's direct pass-through.
func (s *Server) defaultRouteKind() (string, error) {
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
