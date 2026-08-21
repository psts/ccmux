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

// Shared windows on the wire (v2). The daemon a lens talks to (the hub in a
// federation, a lone daemon otherwise) is the window authority: `group` on a
// workspace is the SHARED window name — the same for every caller — and the
// only personal state is each login's open flags, served by /v1/windows.
// See docs/multitenant-plan.md ("v2: shared windows").

// stampShared returns copies of list with Group resolved through the shared
// membership (legacy persisted group until imported) and Owner attributed to
// the owning host's human. A workspace still carrying a legacy group with no
// membership and no marker is imported on read, once — the pre-windows →
// windows migration, which needs no owner: the window it creates is shared.
func (s *Server) stampShared(list []*model.Workspace) []*model.Workspace {
	resolve := s.mgr.SharedGroupResolver()
	// nil (as opposed to empty) means the markers are unreadable: importing
	// blind could resurrect an arrangement someone deliberately cleared, so
	// no imports run this pass.
	imported := s.mgr.ViewImports()
	out := make([]*model.Workspace, len(list))
	for i, ws := range list {
		cp := *ws
		if cp.Group != "" && imported != nil && !imported[cp.ID] {
			if err := s.mgr.SeedWindowMembership(cp.ID, cp.Group); err != nil {
				log.Printf("windows: importing legacy group %q for %s failed: %v", cp.Group, cp.ID, err)
			}
		}
		cp.Owner = s.hostOwner(cp.Host)
		cp.Group = resolve(cp.ID, cp.Group)
		cp.OwnerGroup = ""
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

// workspaceKnown answers "may membership reference this id" against the
// surface this daemon serves: the hub's aggregate, or the local manager.
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

// hubListWorkspaces serves the aggregated, window-stamped GET /v1/workspaces.
func (s *Server) hubListWorkspaces(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.stampShared(s.hub.agg.Aggregate(r.Context())))
}

// putGroup assigns a workspace to the shared window of that name (creating it
// if new); empty removes it from any window. A SHARED edit — every lens sees
// it. Handled at this daemon, never proxied: the daemon lenses talk to is the
// window authority. Always marks the legacy import, so the compat-persisted
// column can never resurrect a deliberate removal.
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
	if err := s.mgr.SeedWindowMembership(id, req.Group); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// listWindows serves GET /v1/windows: every shared window, with a
// caller-relative `open` flag beside the shared openBy list.
func (s *Server) listWindows(w http.ResponseWriter, r *http.Request) {
	login := s.resolveIdentity(r).Login
	type windowResp struct {
		ID           string   `json:"id"`
		Name         string   `json:"name"`
		WorkspaceIDs []string `json:"workspaceIds"`
		OpenBy       []string `json:"openBy"`
		Open         bool     `json:"open"`
	}
	windows := s.mgr.Windows()
	out := make([]windowResp, 0, len(windows))
	for _, win := range windows {
		wr := windowResp{ID: win.ID, Name: win.Name, WorkspaceIDs: win.WorkspaceIDs, OpenBy: win.OpenBy}
		for _, l := range win.OpenBy {
			if l == login {
				wr.Open = true
			}
		}
		out = append(out, wr)
	}
	writeJSON(w, http.StatusOK, out)
}

// setWindowOpen serves POST /v1/windows/{id}/open and /close. A close answers
// {last, members}: when the caller was the final opener, the LENS archives
// the members — the agreed model is that a window nobody has open goes to
// sleep, and the lens already owns the archive loop.
func (s *Server) setWindowOpen(open bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		login := s.resolveIdentity(r).Login
		last, members, err := s.mgr.SetWindowOpen(login, r.PathValue("id"), open)
		if err != nil {
			writeError(w, http.StatusNotFound, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"last": last, "members": members})
	}
}

// renameWindow serves PUT /v1/windows/{id}: a shared rename, refused on a
// name collision.
func (s *Server) renameWindow(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name string `json:"name"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	if err := s.mgr.RenameSharedWindow(r.PathValue("id"), req.Name); err != nil {
		writeError(w, http.StatusConflict, err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// archiveGuard fronts archive and delete: stopping a session is global, so it
// is refused while the session's window is OPEN by someone else, or when the
// caller is not the owner of the session's host. force=1 overrides both — and
// the last-close sleep uses it deliberately, because "nobody has it open" is
// the model's own permission.
func (s *Server) archiveGuard(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("force") == "1" {
			next(w, r)
			return
		}
		id := r.PathValue("id")
		caller := s.resolveIdentity(r).Login
		// An unknown owner ("") falls through to the open check: a genuinely
		// unowned host is permissive by design, and a registry gap still hits
		// the owner route's own 404/502 right after this guard.
		if owner := s.workspaceOwner(r.Context(), id); owner != "" && caller != owner {
			writeError(w, http.StatusConflict, "workspace belongs to "+owner+" — pass force=1 to archive anyway")
			return
		}
		// Fail CLOSED on unreadable tables: "window not open" is this guard's
		// permission to stop a session for everyone, and a DB hiccup must not
		// impersonate it.
		members, opens, names, err := s.mgr.WindowsStrict()
		if err != nil {
			log.Printf("windows: archive guard for %s could not read window state: %v", id, err)
			writeError(w, http.StatusServiceUnavailable, "window state unreadable — retry, or pass force=1 to archive anyway")
			return
		}
		if wid, ok := members[id]; ok {
			for login := range opens[wid] {
				if login != caller {
					writeError(w, http.StatusConflict,
						login+" still has "+names[wid]+" open — pass force=1 to archive anyway")
					return
				}
			}
		}
		next(w, r)
	}
}

// hostCreateRoute wraps the hub's per-host create: a create proxied to a
// REMOTE host succeeds on that host's store, so the hub — the window
// authority — must seed the membership itself from the request's group and
// the response's id. A create the hub runs locally seeds inside
// createWorkspace instead.
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
		// The tee below reads the response bytes as JSON, so the member must
		// not compress them — a gzip'd create response would fail the parse on
		// every cross-host create, silently, forever.
		r.Header.Set("Accept-Encoding", "identity")
		cw := &captureWriter{ResponseWriter: w, status: http.StatusOK}
		proxied(cw, r)
		if cw.status/100 != 2 {
			return
		}
		var ws struct {
			ID string `json:"id"`
		}
		if json.Unmarshal(cw.body.Bytes(), &ws) != nil || ws.ID == "" {
			log.Printf("windows: create on %s returned %d but no workspace id parsed; membership in %q not seeded",
				r.PathValue("host"), cw.status, req.Group)
			return
		}
		if err := s.mgr.SeedWindowMembership(ws.ID, req.Group); err != nil {
			log.Printf("windows: seeding membership for %s failed: %v", ws.ID, err)
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
