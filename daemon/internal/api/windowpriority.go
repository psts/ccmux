package api

import (
	"errors"
	"log"
	"net/http"

	"ccmux.dev/ccmuxd/internal/manager"
)

// taskPriorityOf answers each window's task priority for the list: its
// own number, or nil when it has none, when no runner runs here (nothing
// would honor it) or when the numbers are unreadable.
func (s *Server) taskPriorityOf() func(windowID string) *int {
	none := func(string) *int { return nil }
	if s.taskRunner == nil {
		return none
	}
	own, err := s.mgr.WindowPriorities()
	if err != nil {
		log.Printf("windows: task priorities unreadable (%v); serving without", err)
		return none
	}
	return func(windowID string) *int {
		n, ok := own[windowID]
		if !ok {
			return nil
		}
		return &n
	}
}

// setWindowPriority serves PUT /v1/windows/{id}/priority ({"priority": n}):
// which window's tasks the runner serves first when windows compete for a
// slot, from the Tasks header's picker. 1 is first; 0 clears it.
func (s *Server) setWindowPriority(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Priority *int `json:"priority"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	if req.Priority == nil {
		writeError(w, http.StatusBadRequest, "priority is required (0 for none)")
		return
	}
	err := s.mgr.SetWindowPriority(r.PathValue("id"), *req.Priority)
	switch {
	case err == nil:
		w.WriteHeader(http.StatusNoContent)
	case errors.Is(err, manager.ErrUnknownWindow):
		writeError(w, http.StatusNotFound, err.Error())
	case errors.Is(err, manager.ErrBadPriority):
		writeError(w, http.StatusBadRequest, err.Error())
	default:
		writeError(w, http.StatusServiceUnavailable, err.Error())
	}
}
