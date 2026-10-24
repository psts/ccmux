package api

import (
	"net/http"
	"strings"
	"time"

	"ccmux.dev/ccmuxd/internal/buscontext"
	"ccmux.dev/ccmuxd/internal/manager"
	"ccmux.dev/ccmuxd/internal/repoowns"
	"ccmux.dev/ccmuxd/internal/store"
)

// What each repo owns, as the bus tells every session in a window (see
// repoowns). The line is the small model's summary unless a human typed
// one; both lenses show it and let a human type over it.

// ownsStore is the slice of the registry the manual line needs.
type ownsStore interface {
	RepoOwns(repoPath string) (store.RepoOwns, error)
	SetRepoOwnsManual(repoPath, manual string, now int64) error
}

// maxOwnsManual bounds a typed line: it rides every session's context.
const maxOwnsManual = 200

// SetOwns wires the summaries and the store. Without them every session
// shows no owns line and the routes answer 503.
func (s *Server) SetOwns(svc *repoowns.Service, st ownsStore) {
	s.owns, s.ownsStore = svc, st
}

// sessionsWithOwns is windowSessions with each repo's line, for the text
// the bus hands sessions (not for windowRepos, which needs folders only).
func (s *Server) sessionsWithOwns(win manager.WindowInfo) []buscontext.Session {
	list := s.windowSessions(win)
	if s.owns == nil {
		return list
	}
	for i := range list {
		list[i].Owns = s.owns.Line(list[i].Name, list[i].RepoPath)
	}
	return list
}

// readerOfPane is who a pane's session is: a role agent when its workspace
// is an agent instance, else a repo session named after its workspace. A
// pane this daemon does not host (another host's, or none) reads as a repo
// session named fallback.
func (s *Server) readerOfPane(paneID, fallback string) buscontext.Reader {
	if paneID != "" {
		if ws := s.mgr.Workspace(s.mgr.WorkspaceForPane(paneID)); ws != nil {
			if ws.Agent != "" {
				return buscontext.Reader{Agent: true}
			}
			return buscontext.Reader{Self: repoBusName(ws.Name, ws.RepoPath)}
		}
	}
	return buscontext.Reader{Self: fallback}
}

// workspaceOwns: GET /v1/workspaces/{id}/owns → {"auto","manual","line"}
// for the lens's editor. PUT {"manual"} sets the typed line ("" hands the
// repo back to the summary) and answers the same shape.
func (s *Server) workspaceOwns(w http.ResponseWriter, r *http.Request) {
	repo, ok := s.ownsRepo(w, r)
	if !ok {
		return
	}
	if r.Method == http.MethodPut && !s.setOwnsManual(w, r, repo) {
		return
	}
	o, err := s.ownsStore.RepoOwns(repo)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"auto": o.Auto, "manual": o.Manual, "line": o.Line()})
}

// ownsRepo resolves the route's workspace to its repo folder; ok=false
// means the answer was written.
func (s *Server) ownsRepo(w http.ResponseWriter, r *http.Request) (string, bool) {
	if s.ownsStore == nil {
		writeError(w, http.StatusServiceUnavailable, "owns lines are not configured on this daemon")
		return "", false
	}
	ws := s.mgr.Workspace(r.PathValue("id"))
	if ws == nil {
		writeError(w, http.StatusNotFound, "unknown workspace")
		return "", false
	}
	if ws.Agent != "" {
		writeError(w, http.StatusBadRequest, "an agent's session owns no repo; its description says what it does")
		return "", false
	}
	return ws.RepoPath, true
}

func (s *Server) setOwnsManual(w http.ResponseWriter, r *http.Request, repo string) bool {
	var req struct {
		Manual string `json:"manual"`
	}
	if !decodeJSON(w, r, &req) {
		return false
	}
	line := strings.Join(strings.Fields(req.Manual), " ")
	if len([]rune(line)) > maxOwnsManual {
		writeError(w, http.StatusBadRequest, "keep it to one short line (200 characters at most)")
		return false
	}
	if err := s.ownsStore.SetRepoOwnsManual(repo, line, time.Now().UnixMilli()); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return false
	}
	return true
}
