package api

import (
	"encoding/json"
	"errors"
	"net/http"
)

// maxScratchpad bounds one scratchpad. The largest the Mac app ever wrote was
// about 21 KB; a megabyte is room for notes, not for a file dump.
const maxScratchpad = 1 << 20

// Scratchpad routes: a workspace's shared notes, one copy for everyone.
//
// Handled at this daemon, never proxied, like the window routes: the daemon
// lenses talk to keeps the notes for every workspace it lists, whichever host
// runs it. So a workspace on a Mac still on an older ccmuxd has notes too,
// and they live where everybody's lens reads them. The id is checked against
// what this daemon serves, which for a hub misses a host that is unreachable
// right now: its workspaces 404 until it is back.

// getScratchpad serves GET /v1/workspaces/{id}/scratchpad.
func (s *Server) getScratchpad(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !s.workspaceKnown(r.Context(), id) {
		writeError(w, http.StatusNotFound, "unknown workspace "+id)
		return
	}
	pad, err := s.mgr.Scratchpad(id)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, pad)
}

// putScratchpad serves PUT /v1/workspaces/{id}/scratchpad with {"content"}.
// The last save wins, as it did in the Mac app; every lens hears of it.
func (s *Server) putScratchpad(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !s.workspaceKnown(r.Context(), id) {
		writeError(w, http.StatusNotFound, "unknown workspace "+id)
		return
	}
	var req struct {
		Content *string `json:"content"`
	}
	err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxScratchpad+4096)).Decode(&req)
	if code, msg := scratchpadBodyError(err, req.Content); code != 0 {
		writeError(w, code, msg)
		return
	}
	pad, err := s.mgr.SaveScratchpad(id, *req.Content, s.resolveIdentity(r).Login)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, pad)
}

// scratchpadBodyError judges a PUT body: too big, unreadable, or missing
// "content" (an empty string is a cleared pad; an absent field is a mistake
// that must not wipe one).
func scratchpadBodyError(err error, content *string) (int, string) {
	var tooBig *http.MaxBytesError
	switch {
	case errors.As(err, &tooBig):
		return http.StatusRequestEntityTooLarge, "scratchpad is larger than 1 MB"
	case err != nil:
		return http.StatusBadRequest, "invalid JSON: " + err.Error()
	case content == nil:
		return http.StatusBadRequest, `missing "content"`
	case len(*content) > maxScratchpad:
		return http.StatusRequestEntityTooLarge, "scratchpad is larger than 1 MB"
	}
	return 0, ""
}
