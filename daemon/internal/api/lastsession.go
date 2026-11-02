package api

import (
	"context"
	"net/http"

	"ccmux.dev/ccmuxd/internal/model"
)

// lastSession serves GET /v1/me/last-session: the session the caller last
// typed in, on any lens, and when. A phone opens on it, so picking up the
// phone lands where the computer left off. An empty workspaceId means the
// caller has typed in none of the sessions this daemon serves (or has no
// login to be known by).
func (s *Server) lastSession(w http.ResponseWriter, r *http.Request) {
	login := s.resolveIdentity(r).Login
	ws, at := lastDriven(s.servedWorkspaces(r.Context()), login, s.focus.DriverLogin)
	writeJSON(w, http.StatusOK, map[string]any{"workspaceId": ws, "at": at})
}

// servedWorkspaces is every session this daemon serves its lenses: on a hub,
// its members' too (the same list GET /v1/workspaces serves there), so a
// session hosted on another machine counts; s.focus answers drivers hub-wide
// to match (federatedFocus).
func (s *Server) servedWorkspaces(ctx context.Context) []*model.Workspace {
	if s.hub != nil {
		return s.hub.agg.Aggregate(ctx)
	}
	return s.mgr.List()
}

// lastDriven is the workspace login typed in most recently, by the same
// driver record push routing uses (presenceHub.DriverLogin: the live typist,
// else the remembered one), and when. "" for an unknown or anonymous login.
func lastDriven(list []*model.Workspace, login string, driver func(wsID string) (string, int64, bool)) (string, int64) {
	if login == "" || login == "anon" {
		return "", 0
	}
	best, bestAt := "", int64(0)
	for _, ws := range list {
		who, at, ok := driver(ws.ID)
		if ok && who == login && at > bestAt {
			best, bestAt = ws.ID, at
		}
	}
	return best, bestAt
}
