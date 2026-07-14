// Package api serves ccmuxd's REST + WebSocket surface: the wire contract shared
// by every lens (native app, web, phone).
package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"

	"github.com/gorilla/websocket"

	"ccmux.dev/ccmuxd/internal/manager"
	"ccmux.dev/ccmuxd/internal/tailnet"
	"ccmux.dev/ccmuxd/web"
)

// Server adapts a Manager to HTTP.
type Server struct {
	mgr      *manager.Manager
	presence *presenceHub
	identity *tailnet.Resolver
	upgrader websocket.Upgrader

	// Push notifications, wired by EnablePush; nil when push is disabled (the
	// /v1/push/* handlers then answer 503 and no notifier runs).
	sender    pushSender
	pushStore pushStore
}

func NewServer(mgr *manager.Manager) *Server {
	return &Server{
		mgr:      mgr,
		presence: newPresenceHub(mgr),
		identity: tailnet.NewResolver(),
		// Same-origin default; the web lens is served from this daemon, and
		// tailnet identity gates access. Loosened checks come with auth.
		upgrader: websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }},
	}
}

// EnablePush wires Web Push: it stores the sender + subscription store the
// /v1/push/* handlers use, and starts a notifier that pushes on attention (with
// per-dev suppression) for the lifetime of ctx. Idempotent-safe to call once at
// startup; the notifier's firehose subscription is released when ctx is cancelled.
func (s *Server) EnablePush(ctx context.Context, sender pushSender, ps pushStore) {
	s.sender = sender
	s.pushStore = ps
	n := &notifier{sender: sender, subs: ps, focus: s.presence, names: s.mgr}
	id, ch := s.mgr.SubscribeEvents()
	go func() {
		defer s.mgr.UnsubscribeEvents(id)
		n.run(ctx, ch)
	}()
}

// Handler builds the routed HTTP handler (Go 1.22+ method+wildcard patterns).
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/health", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
	})
	mux.HandleFunc("GET /v1/workspaces", s.listWorkspaces)
	mux.HandleFunc("POST /v1/workspaces", s.createWorkspace)
	mux.HandleFunc("DELETE /v1/workspaces/{id}", s.deleteWorkspace)
	mux.HandleFunc("POST /v1/workspaces/{id}/panes", s.spawnPane)
	mux.HandleFunc("POST /v1/workspaces/{id}/revive", s.reviveWorkspace)
	mux.HandleFunc("PUT /v1/workspaces/{id}/layout", s.putLayout)
	mux.HandleFunc("GET /v1/panes/{id}/driver", s.paneDriver)
	mux.HandleFunc("GET /v1/push/vapid", s.pushVAPID)
	mux.HandleFunc("GET /v1/push/subscriptions", s.listSubscriptions)
	mux.HandleFunc("POST /v1/push/subscriptions", s.createSubscription)
	mux.HandleFunc("DELETE /v1/push/subscriptions", s.deleteSubscription)
	mux.HandleFunc("GET /v1/attach", s.attach)
	mux.HandleFunc("GET /v1/events", s.events)
	// The web lens (served from the embedded bundle) catches everything not
	// matched by a more specific /v1 pattern.
	mux.Handle("GET /", http.FileServerFS(web.Files))
	return mux
}

func (s *Server) listWorkspaces(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, s.mgr.List())
}

type createWorkspaceReq struct {
	Name           string `json:"name"`
	RepoPath       string `json:"repoPath"`
	CWD            string `json:"cwd"`
	StartupCommand string `json:"startupCommand"`
	CreatedBy      string `json:"createdBy"`
}

func (s *Server) createWorkspace(w http.ResponseWriter, r *http.Request) {
	var req createWorkspaceReq
	if !decodeJSON(w, r, &req) {
		return
	}
	if req.RepoPath == "" {
		writeError(w, http.StatusBadRequest, "repoPath required")
		return
	}
	ws, err := s.mgr.CreateWorkspace(req.Name, req.RepoPath, req.CWD, req.StartupCommand, req.CreatedBy)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, ws)
}

func (s *Server) deleteWorkspace(w http.ResponseWriter, r *http.Request) {
	if err := s.mgr.KillWorkspace(r.PathValue("id")); err != nil {
		writeError(w, http.StatusNotFound, err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

type spawnPaneReq struct {
	CWD            string `json:"cwd"`
	StartupCommand string `json:"startupCommand"`
	CreatedBy      string `json:"createdBy"`
}

func (s *Server) spawnPane(w http.ResponseWriter, r *http.Request) {
	var req spawnPaneReq
	if !decodeJSON(w, r, &req) {
		return
	}
	p, err := s.mgr.SpawnPane(r.PathValue("id"), req.CWD, req.StartupCommand, req.CreatedBy)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, p)
}

func (s *Server) reviveWorkspace(w http.ResponseWriter, r *http.Request) {
	ws, err := s.mgr.ReviveWorkspace(r.PathValue("id"))
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, ws)
}

type layoutReq struct {
	Blob        string `json:"blob"`
	BaseVersion int    `json:"baseVersion"`
}

// putLayout stores a workspace's opaque layout blob under optimistic concurrency.
// A stale baseVersion returns 409 with the current {version, blob} so the client
// can rebase; success returns {version} and broadcasts the change to other lenses.
func (s *Server) putLayout(w http.ResponseWriter, r *http.Request) {
	var req layoutReq
	if !decodeJSON(w, r, &req) {
		return
	}
	id := r.PathValue("id")
	newV, err := s.mgr.SetLayout(id, req.Blob, req.BaseVersion)
	switch {
	case errors.Is(err, manager.ErrLayoutConflict):
		cur := s.mgr.Workspace(id)
		blob := ""
		if cur != nil {
			blob = cur.LayoutJSON
		}
		writeJSON(w, http.StatusConflict, map[string]any{"version": newV, "blob": blob})
	case err != nil:
		writeError(w, http.StatusNotFound, err.Error())
	default:
		writeJSON(w, http.StatusOK, map[string]any{"version": newV})
	}
}

// paneDriver reports the human currently driving a pane's workspace, for the git
// co-author trailer. 404 if the pane is unknown; 204 when nobody is driving (a
// solo/unattended session), so the hook simply adds no trailer.
func (s *Server) paneDriver(w http.ResponseWriter, r *http.Request) {
	wsID := s.mgr.WorkspaceForPane(r.PathValue("id"))
	if wsID == "" {
		writeError(w, http.StatusNotFound, "unknown pane")
		return
	}
	driver, ok := s.presence.Driver(wsID)
	if !ok {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	writeJSON(w, http.StatusOK, driver)
}

// --- JSON helpers ---

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

func decodeJSON(w http.ResponseWriter, r *http.Request, v any) bool {
	if err := json.NewDecoder(r.Body).Decode(v); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return false
	}
	return true
}
