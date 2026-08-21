package api

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log"
	"net/http"

	"ccmux.dev/ccmuxd/internal/model"
)

// Per-user views on the wire. The daemon a lens talks to (the hub in a
// federation, a lone daemon otherwise) is the view authority: it stamps each
// workspace's Group with THE CALLER's view row, Owner with the owning host's
// human, and OwnerGroup with that human's row — and it handles the group PUT
// against its own store instead of proxying. See docs/multitenant-plan.md.

// stampViews returns copies of list with the per-caller view fields filled.
// A workspace still carrying a legacy persisted group but no view rows is
// imported on read: its arrangement becomes the host owner's row, once —
// that is the whole pre-views → views migration.
func (s *Server) stampViews(list []*model.Workspace, caller string) []*model.Workspace {
	views := s.mgr.Views()
	// nil (as opposed to empty) means the markers are unreadable: importing
	// blind could resurrect an arrangement someone deliberately put away, so
	// no imports run this pass.
	imported := s.mgr.ViewImports()
	out := make([]*model.Workspace, len(list))
	for i, ws := range list {
		cp := *ws
		owner := s.hostOwner(cp.Host)
		rows := views[cp.ID]
		if len(rows) == 0 && cp.Group != "" && owner != "" && imported != nil && !imported[cp.ID] {
			if err := s.mgr.ImportView(owner, cp.ID, cp.Group); err == nil {
				rows = map[string]string{owner: cp.Group}
			} else {
				log.Printf("views: importing legacy group %q for %s failed: %v", cp.Group, cp.ID, err)
			}
		}
		cp.Owner = owner
		cp.OwnerGroup = rows[owner]
		// Compat: an UNOWNED host with no rows keeps its legacy shared group on
		// the wire — an upgrade must not collapse a single-user sidebar into
		// Available before anyone has typed their email. Multi-tenancy switches
		// on per host the moment its owner is set (and the import runs).
		if owner != "" || len(rows) > 0 {
			cp.Group = rows[caller]
		}
		out[i] = &cp
	}
	return out
}

// hostOwner is the owner login of a host in the federation; "" (this daemon's
// own owner) also covers the standalone case, where nothing stamps Host.
func (s *Server) hostOwner(hostID string) string {
	if s.hub == nil || hostID == "" || hostID == s.hub.selfID {
		return s.mgr.Owner()
	}
	if host, ok := s.hub.reg.Get(hostID); ok {
		return host.Owner
	}
	return ""
}

// workspaceKnown answers "may a view row reference this id" against the surface
// this daemon serves: the hub's aggregate, or the local manager.
func (s *Server) workspaceKnown(ctx context.Context, id string) bool {
	if s.hub != nil {
		_, ok := s.hub.agg.OwnerOrRefresh(ctx, id)
		return ok
	}
	return s.mgr.Workspace(id) != nil
}

// workspaceOwner is the owner login of the workspace's host ("" when unknown).
func (s *Server) workspaceOwner(ctx context.Context, id string) string {
	if s.hub == nil {
		return s.mgr.Owner()
	}
	hostID, ok := s.hub.agg.OwnerOrRefresh(ctx, id)
	if !ok {
		return ""
	}
	return s.hostOwner(hostID)
}

// hubListWorkspaces serves the aggregated, view-stamped GET /v1/workspaces.
func (s *Server) hubListWorkspaces(w http.ResponseWriter, r *http.Request) {
	list := s.hub.agg.Aggregate(r.Context())
	writeJSON(w, http.StatusOK, s.stampViews(list, s.resolveIdentity(r).Login))
}

// putGroup writes THE CALLER's view row for a workspace: which of their windows
// it sits in. An empty group deletes the row (they put it away). Handled at
// this daemon — never proxied — because the view authority is the daemon the
// lens talks to, not the host the workspace runs on.
func (s *Server) putGroup(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Group string `json:"group"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	id := r.PathValue("id")
	if !s.workspaceKnown(r.Context(), id) {
		writeError(w, http.StatusNotFound, "unknown workspace "+id)
		return
	}
	if err := s.mgr.SetView(s.resolveIdentity(r).Login, id, req.Group); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// archiveGuard fronts POST /v1/workspaces/{id}/archive: archiving stops a
// session for EVERYONE, so it is refused when the caller is not the owner, or
// when any other login still keeps the workspace in a window. force=1
// overrides both ("Archive anyway"); an unowned host has no owner to check,
// which preserves single-user behavior exactly.
func (s *Server) archiveGuard(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("force") == "1" {
			next(w, r)
			return
		}
		id := r.PathValue("id")
		caller := s.resolveIdentity(r).Login
		if owner := s.workspaceOwner(r.Context(), id); owner != "" && caller != owner {
			writeError(w, http.StatusConflict, "workspace belongs to "+owner+" — pass force=1 to archive anyway")
			return
		}
		for login := range s.mgr.Views()[id] {
			if login != caller {
				writeError(w, http.StatusConflict, login+" still has this workspace in a window — pass force=1 to archive anyway")
				return
			}
		}
		next(w, r)
	}
}

// hostCreateRoute wraps the hub's per-host create: a create proxied to a REMOTE
// host succeeds on that host's store, so the hub — the view authority — must
// seed the creator's view row itself from the request's group and the response's
// id. A create the hub runs locally seeds inside createWorkspace instead.
func (s *Server) hostCreateRoute() http.HandlerFunc {
	proxied := s.hub.hostScoped(s.createWorkspace, "/v1/workspaces")
	return func(w http.ResponseWriter, r *http.Request) {
		if r.PathValue("host") == s.hub.selfID {
			proxied(w, r)
			return
		}
		body, err := io.ReadAll(r.Body)
		if err != nil {
			writeError(w, http.StatusBadRequest, "read body: "+err.Error())
			return
		}
		r.Body = io.NopCloser(bytes.NewReader(body))
		var req struct {
			Group string `json:"group"`
		}
		if json.Unmarshal(body, &req) != nil || req.Group == "" {
			proxied(w, r)
			return
		}
		login := s.resolveIdentity(r).Login // before the proxy consumes the request
		cw := &captureWriter{ResponseWriter: w, status: http.StatusOK}
		proxied(cw, r)
		if cw.status/100 != 2 {
			return
		}
		var ws struct {
			ID string `json:"id"`
		}
		if json.Unmarshal(cw.body.Bytes(), &ws) == nil && ws.ID != "" {
			if err := s.mgr.SetView(login, ws.ID, req.Group); err != nil {
				log.Printf("views: seeding creator's row for %s failed: %v", ws.ID, err)
			}
		}
	}
}

// captureWriter tees a response so hostCreateRoute can read the created
// workspace's id after the reverse proxy has written it through.
type captureWriter struct {
	http.ResponseWriter
	status int
	body   bytes.Buffer
}

func (c *captureWriter) WriteHeader(code int) {
	c.status = code
	c.ResponseWriter.WriteHeader(code)
}

func (c *captureWriter) Write(b []byte) (int, error) {
	c.body.Write(b)
	return c.ResponseWriter.Write(b)
}
