package api

import (
	"net/http"
)

// withQuietNotices appends the quiet notices waiting for the agent on
// paneID to a start's first prompt, and returns the last seq they cover
// for markQuietNotices once the start went through. A start with no
// prompt takes none: the notices then ride along with what reaches it next.
func (s *Server) withQuietNotices(paneID, prompt string) (string, int64) {
	if s.peersSvc == nil || prompt == "" {
		return prompt, 0
	}
	text, upto := s.peersSvc.NoticesForPane(paneID)
	if text == "" {
		return prompt, 0
	}
	return prompt + "\n\n" + text, upto
}

func (s *Server) markQuietNotices(paneID string, upto int64) {
	if s.peersSvc != nil && upto != 0 {
		s.peersSvc.MarkNoticesForPane(paneID, upto)
	}
}

// paneNotices: POST /v1/panes/{id}/notices hands over the quiet notices
// waiting for the session on the pane, marking them once the answer is out, as {"text": "..."}, "" when
// none wait. ccmux-notify.sh calls it on UserPromptSubmit, so a repo session
// reads its notices inside the turn its human just started. Gated like every
// pane route that acts as the pane's peer: loopback AND the pane's token.
func (s *Server) paneNotices(w http.ResponseWriter, r *http.Request) {
	if !requireLoopback(w, r) || !s.peersEnabled(w) {
		return
	}
	paneID := r.PathValue("id")
	if tok := bearerToken(r); tok == "" || !s.peersSvc.AuthorizePane(paneID, tok) {
		writeError(w, http.StatusUnauthorized, "pane token required (CCMUX_PANE_TOKEN in the pane's environment)")
		return
	}
	text, upto := s.peersSvc.NoticesForPane(paneID)
	writeJSON(w, http.StatusOK, map[string]string{"text": text})
	// Marked only once the answer is out: a hook that gave up (its curl has a
	// two-second cap) leaves the notices waiting for the next handover.
	if http.NewResponseController(w).Flush() != nil || r.Context().Err() != nil {
		return
	}
	s.markQuietNotices(paneID, upto)
}
