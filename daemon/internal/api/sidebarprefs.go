package api

import (
	"errors"
	"net/http"

	"ccmux.dev/ccmuxd/internal/manager"
)

// Each person's own thread-page sidebar: which groups are folded into their
// header line and the order of the groups. Keyed by the caller's login
// (resolveIdentity), so it follows a person to every computer and the phone.
// It is a view and nothing else: folding a group never touches its
// sessions, unlike a window's open flag. A caller the daemon cannot name
// is "anon", and every such caller shares that one row set.

// sidebarPrefs serves GET /v1/me/sidebar: {"groups": [{windowId,
// collapsed, position}]}, placed groups first; position is null for a
// group the caller never placed.
func (s *Server) sidebarPrefs(w http.ResponseWriter, r *http.Request) {
	list, err := s.mgr.SidebarPrefs(s.resolveIdentity(r).Login)
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"groups": list})
}

// setSidebarCollapsed serves PUT /v1/me/sidebar/{id} ({"collapsed": bool}):
// fold or unfold one group for the caller. 404 for a window that does not
// exist.
func (s *Server) setSidebarCollapsed(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Collapsed *bool `json:"collapsed"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	if req.Collapsed == nil {
		writeError(w, http.StatusBadRequest, "collapsed is required (true or false)")
		return
	}
	err := s.mgr.SetSidebarCollapsed(s.resolveIdentity(r).Login, r.PathValue("id"), *req.Collapsed)
	switch {
	case err == nil:
		w.WriteHeader(http.StatusNoContent)
	case errors.Is(err, manager.ErrUnknownWindow):
		writeError(w, http.StatusNotFound, err.Error())
	default:
		writeError(w, http.StatusServiceUnavailable, err.Error())
	}
}

// setSidebarOrder serves PUT /v1/me/sidebar/order ({"windowIds": [...]}):
// the caller's whole order in one write, first on top. Groups left out
// keep the default order after the listed ones; an id that is no window
// (deleted since the page drew) is skipped. An empty or repeated id is a
// 400: that is a broken page, not a stale one.
func (s *Server) setSidebarOrder(w http.ResponseWriter, r *http.Request) {
	var req struct {
		WindowIDs []string `json:"windowIds"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	if msg := badOrder(req.WindowIDs); msg != "" {
		writeError(w, http.StatusBadRequest, msg)
		return
	}
	if err := s.mgr.SetSidebarOrder(s.resolveIdentity(r).Login, req.WindowIDs); err != nil {
		writeError(w, http.StatusServiceUnavailable, err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// badOrder says what is wrong with an order list, "" when nothing is.
func badOrder(ids []string) string {
	if ids == nil {
		return "windowIds is required (a list, first on top)"
	}
	seen := map[string]bool{}
	for _, id := range ids {
		if id == "" {
			return "windowIds holds an empty id"
		}
		if seen[id] {
			return "windowIds names " + id + " twice"
		}
		seen[id] = true
	}
	return ""
}
