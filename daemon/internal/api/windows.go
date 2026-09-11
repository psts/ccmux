package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"time"

	"ccmux.dev/ccmuxd/internal/manager"
	"ccmux.dev/ccmuxd/internal/model"
	"ccmux.dev/ccmuxd/internal/store"
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
		// Both guards, deliberately: the marker (a removal must stick) AND
		// existing membership (an arrangement someone made is never imported
		// over, even where v1 forgot to write markers).
		if cp.Group != "" && imported != nil && !imported[cp.ID] && !s.mgr.HasMembership(cp.ID) {
			if err := s.mgr.SeedWindowMembership(cp.ID, cp.Group); err != nil {
				log.Printf("windows: importing legacy group %q for %s failed: %v", cp.Group, cp.ID, err)
			}
		}
		cp.Owner = s.hostOwner(cp.Host)
		cp.Group = resolve(cp.ID, cp.Group)
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
// caller-relative `open` flag beside the shared openBy list. Strict read,
// 503 on error: lenses ACT on this list (close decisions hang off it), and a
// 200-empty from unreadable tables would defeat every keep-last-list fallback
// while a person's open flag quietly blocks everyone else's archives.
func (s *Server) listWindows(w http.ResponseWriter, r *http.Request) {
	login := s.resolveIdentity(r).Login
	type windowResp struct {
		ID           string   `json:"id"`
		Name         string   `json:"name"`
		WorkspaceIDs []string `json:"workspaceIds"`
		OpenBy       []string `json:"openBy"`
		Open         bool     `json:"open"`
		// OpenHere is THIS lens's own row, when it names a device. Open is
		// per-login and so cannot answer it: a window your phone holds open
		// reads Open=true on your Mac, which is how a window with no local
		// counterpart became invisible AND unopenable.
		OpenHere bool `json:"openHere"`
	}
	windows, err := s.mgr.WindowsListStrict()
	if err != nil {
		log.Printf("windows: listing failed: %v", err)
		writeError(w, http.StatusServiceUnavailable, "window state unreadable — retry")
		return
	}
	here, err := s.mgr.DeviceOpenWindows(login, r.URL.Query().Get("device"))
	if err != nil {
		// Same answer as the list read above, for the same reason: serving
		// openHere=false on an unreadable table is indistinguishable from
		// "this lens holds nothing open", and both lenses list a window as
		// CLOSED on that. Clicking it then opens a second window onto a shared
		// one this lens already has. A 503 makes them keep their last list.
		log.Printf("windows: device open set unreadable: %v", err)
		writeError(w, http.StatusServiceUnavailable, "window state unreadable — retry")
		return
	}
	out := make([]windowResp, 0, len(windows))
	for _, win := range windows {
		wr := windowResp{ID: win.ID, Name: win.Name, WorkspaceIDs: win.WorkspaceIDs,
			OpenBy: win.OpenBy, OpenHere: here[win.ID]}
		for _, l := range win.OpenBy {
			if l == login {
				wr.Open = true
			}
		}
		out = append(out, wr)
	}
	writeJSON(w, http.StatusOK, out)
}

// syncWindowOpen serves POST /v1/windows/open-set: one lens declaring the
// WHOLE set of windows it currently has open. The daemon makes that device's
// rows match — asserting the ones listed, dropping the ones not.
//
// This is the repair path, and it is why the flag is keyed by device. Until it
// existed a lens could only ever ADD flags: a close that never landed (quit,
// crash, an unreachable daemon, a name that no longer matched) left a row
// nothing could clear, which blocked the archive-on-last-close rule forever.
// Scoped to the caller's own device, so declaring your set can never close a
// window another lens is showing.
func (s *Server) syncWindowOpen(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Device    string   `json:"device"`
		Label     string   `json:"deviceLabel"`
		WindowIDs []string `json:"windowIds"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "body must be {device, deviceLabel, windowIds}")
		return
	}
	if req.Device == "" {
		// Without a device there is nothing to scope the clear to, and doing it
		// by login would close windows this lens knows nothing about.
		writeError(w, http.StatusBadRequest, "device is required: the set is per-lens, not per-login")
		return
	}
	login := s.resolveIdentity(r).Login
	now := time.Now().UnixMilli()
	// An id the daemon has never heard of is the CALLER's problem and is safe to
	// skip: a lens whose list still holds a window pruned elsewhere is routine.
	// A store failure is not safe to skip. Asserting is what earns the right to
	// clear, so if any assert genuinely failed the clear must not run at all —
	// otherwise a declaration that only half landed deletes the rest, and the
	// lens is told it succeeded. The next lens to close then sees no openers,
	// answers last=true, and force-archives sessions that are on screen.
	var unwritten int
	for _, id := range req.WindowIDs {
		_, _, err := s.mgr.SetWindowOpen(store.WindowOpenFlag{
			Login: login, WindowID: id, Device: req.Device, Label: req.Label, Seen: now,
		}, true)
		if err == nil {
			continue
		}
		log.Printf("windows: open-set: %s: %v", id, err)
		if !errors.Is(err, manager.ErrUnknownWindow) {
			unwritten++
		}
	}
	if unwritten > 0 {
		writeError(w, http.StatusServiceUnavailable, "window state unwritable — retry")
		return
	}
	if err := s.mgr.ClearOwnStaleOpens(login, req.Device, req.WindowIDs); err != nil {
		// Logged as well as answered, the way listWindows does: the wire string
		// is generic and this is the only record of which store call failed.
		log.Printf("windows: open-set: clearing %s's stale rows: %v", req.Device, err)
		writeError(w, http.StatusServiceUnavailable, "window state unwritable — retry")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// setWindowOpen serves POST /v1/windows/{id}/open and /close. A close answers
// {last, members}: when the caller was the final opener, the LENS archives
// the members — the agreed model is that a window nobody has open goes to
// sleep, and the lens already owns the archive loop.
func (s *Server) setWindowOpen(open bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		login := s.resolveIdentity(r).Login
		// Device and label come from the lens. A lens that sends neither keeps
		// the old row scoping — one row per login, and a close clears every row
		// for that login and window — but not the old lifetime: every open now
		// stamps last_seen, and such a lens has no way to re-assert, so a window
		// it leaves open ages out after the TTL.
		last, members, err := s.mgr.SetWindowOpen(store.WindowOpenFlag{
			Login:    login,
			WindowID: r.PathValue("id"),
			Device:   r.URL.Query().Get("device"),
			Label:    r.URL.Query().Get("deviceLabel"),
		}, open)
		if err != nil {
			// 404 only for what the caller got wrong; a store failure is a
			// 503, or a lens takes the wrong branch off the status code.
			if errors.Is(err, manager.ErrUnknownWindow) {
				writeError(w, http.StatusNotFound, err.Error())
			} else {
				writeError(w, http.StatusServiceUnavailable, err.Error())
			}
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
		// The Mac reverts a local rename on 409 (a collision is user-fixable);
		// a store failure must not wear that costume.
		if errors.Is(err, manager.ErrNameTaken) {
			writeError(w, http.StatusConflict, err.Error())
		} else {
			writeError(w, http.StatusServiceUnavailable, err.Error())
		}
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
