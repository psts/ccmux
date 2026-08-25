package api

import (
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"

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
	if msg := s.rejectRouteDialect(paneID, req.Route); msg != "" {
		writeError(w, http.StatusBadRequest, msg)
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

// llmAccountModels lists the models a named account's upstream serves — the
// data behind the settings tab's alias-target picker. Upstream trouble is a
// 502 with the reason; an unknown account a 404.
func (s *Server) llmAccountModels(w http.ResponseWriter, r *http.Request) {
	models, err := s.llm.UpstreamModels(r.PathValue("name"))
	if err != nil {
		code := http.StatusBadGateway
		if errors.Is(err, llmproxy.ErrUnknownAccount) {
			code = http.StatusNotFound
		}
		writeError(w, code, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"models": models})
}

// kindAllowed is THE compatibility rule, read from a harness's resolved
// AccountKinds: empty means any kind EXCEPT codex — a codex account's
// upstream answers only the codex dialect, so it never serves a harness that
// didn't declare it.
func kindAllowed(kinds []string, kind string) bool {
	if len(kinds) == 0 {
		return kind != "codex"
	}
	return slices.Contains(kinds, kind)
}

// harnessKinds resolves a harness name's allowed account kinds. nil = no
// restriction: a shell pane, a stale/unknown name, or a harness that
// declares nothing.
func (s *Server) harnessKinds(name string) []string {
	if name == "" || s.mgr.Harnesses == nil {
		return nil
	}
	h, err := s.mgr.Harnesses.Resolve(name)
	if err != nil {
		return nil
	}
	return h.AccountKinds
}

// rejectRouteDialect refuses a route whose account cannot serve what the
// pane runs, at click time instead of as 404s in the pane. Returns the
// refusal, "" to allow.
func (s *Server) rejectRouteDialect(paneID, route string) string {
	if route == "" {
		return ""
	}
	kind, err := s.llm.KindOf(route)
	if err != nil || kind == "" {
		return "" // unknown account or unreadable list: SetPaneRoute refuses loudly
	}
	h := s.mgr.HarnessForPane(paneID)
	kinds := s.harnessKinds(h)
	if kindAllowed(kinds, kind) {
		return ""
	}
	if kind == "codex" {
		return fmt.Sprintf("account %q is a codex account, which only serves codex panes", route)
	}
	return fmt.Sprintf("this pane runs %s, which pairs with %s llm accounts — account %q is %s", h, strings.Join(kinds, " or "), route, kind)
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
	// The picker only offers accounts that can actually serve this pane's
	// harness — an impossible choice hidden beats one refused.
	kinds := s.harnessKinds(s.mgr.HarnessForPane(paneID))
	names := make([]string, 0, len(accs))
	for _, a := range accs {
		if kindAllowed(kinds, a.Kind) {
			names = append(names, a.Name)
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"pane": paneID, "route": explicit, "effective": effective, "accounts": names,
	})
}
