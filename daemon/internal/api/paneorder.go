package api

import (
	"errors"
	"net/http"

	"ccmux.dev/ccmuxd/internal/manager"
)

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
		code := http.StatusBadRequest
		if errors.Is(err, manager.ErrUnknownWorkspace) {
			code = http.StatusNotFound
		}
		writeError(w, code, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, ws)
}
