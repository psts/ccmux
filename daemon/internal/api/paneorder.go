package api

import (
	"errors"
	"net/http"

	"ccmux.dev/ccmuxd/internal/manager"
)

// writeWorkspaceError maps a manager error to its status: an unknown id is a
// 404, anything else a bad request. One place, so the next handler that
// returns ErrUnknownWorkspace cannot forget the 404.
func writeWorkspaceError(w http.ResponseWriter, err error) {
	code := http.StatusBadRequest
	if errors.Is(err, manager.ErrUnknownWorkspace) {
		code = http.StatusNotFound
	}
	writeError(w, code, err.Error())
}

type paneOrderReq struct {
	Order []string `json:"order"`
}

// putPaneOrder sets a workspace's shared tab order from a list of pane ids
// (see manager.ReorderPanes for how a partial or stale list is read). Replies
// with the workspace as now served; the change reaches other lenses as a
// workspace-status event on the firehose.
func (s *Server) putPaneOrder(w http.ResponseWriter, r *http.Request) {
	var req paneOrderReq
	if !decodeJSON(w, r, &req) {
		return
	}
	if len(req.Order) == 0 {
		writeError(w, http.StatusBadRequest, "order must list at least one pane id")
		return
	}
	ws, err := s.mgr.ReorderPanes(r.PathValue("id"), req.Order)
	if err != nil {
		writeWorkspaceError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, ws)
}
