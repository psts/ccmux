// Package api serves ccmuxd's REST + WebSocket surface: the wire contract shared
// by every lens (native app, web, phone).
package api

import (
	"encoding/json"
	"net/http"

	"github.com/gorilla/websocket"

	"ccmux.dev/ccmuxd/internal/manager"
	"ccmux.dev/ccmuxd/web"
)

// Server adapts a Manager to HTTP.
type Server struct {
	mgr      *manager.Manager
	upgrader websocket.Upgrader
}

func NewServer(mgr *manager.Manager) *Server {
	return &Server{
		mgr: mgr,
		// Same-origin default; the web lens is served from this daemon, and
		// tailnet identity (Phase 3) gates access. Loosened checks come with auth.
		upgrader: websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }},
	}
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
	mux.HandleFunc("GET /v1/attach", s.attach)
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
