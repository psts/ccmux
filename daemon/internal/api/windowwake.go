package api

import (
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"

	"ccmux.dev/ccmuxd/internal/manager"
	"ccmux.dev/ccmuxd/internal/store"
)

// openFlag is the open-flag row a lens's open or close names. Device and
// label come from the lens. A lens that sends neither keeps the old row
// scoping — one row per login, and a close clears every row for that login
// and window — but not the old lifetime: every open now stamps last_seen,
// and such a lens has no way to re-assert, so a window it leaves open ages
// out after the TTL.
func (s *Server) openFlag(r *http.Request) store.WindowOpenFlag {
	return store.WindowOpenFlag{
		Login:    s.resolveIdentity(r).Login,
		WindowID: r.PathValue("id"),
		Device:   r.URL.Query().Get("device"),
		Label:    r.URL.Query().Get("deviceLabel"),
	}
}

// writeWindowErr answers a failed open or close: 404 only for what the
// caller got wrong; a store failure is a 503, or a lens takes the wrong
// branch off the status code.
func writeWindowErr(w http.ResponseWriter, err error) {
	if errors.Is(err, manager.ErrUnknownWindow) {
		writeError(w, http.StatusNotFound, err.Error())
		return
	}
	writeError(w, http.StatusServiceUnavailable, err.Error())
}

// windowOpenRoute serves POST /v1/windows/{id}/open. It answers wake: the
// members marked awake when the window was last closed, for the lens to wake
// the ones asleep (manager windowwake.go). The marks are read before the
// open, so an unreadable store refuses the open rather than opening a
// window with nothing to say about its sessions.
func (s *Server) windowOpenRoute(w http.ResponseWriter, r *http.Request) {
	f := s.openFlag(r)
	wake, err := s.mgr.WindowWake(f.WindowID)
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, "wake marks unreadable — retry: "+err.Error())
		return
	}
	if _, _, err := s.mgr.SetWindowOpen(f, true); err != nil {
		writeWindowErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"last": false, "members": nil, "wake": wake})
}

// windowCloseRoute serves POST /v1/windows/{id}/close. It answers {last,
// members}: when the caller was the final opener, the LENS archives the
// members — the agreed model is that a window nobody has open goes to sleep,
// and the lens already owns the archive loop. An optional body {awake: [ids]}
// names the members awake now; they are marked before the close, so a mark
// that cannot be saved refuses the close and the lens can try again. No body
// (the Mac app) leaves the marks as they are.
func (s *Server) windowCloseRoute(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Awake *[]string `json:"awake"`
	}
	err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&body)
	if err != nil && !errors.Is(err, io.EOF) {
		writeError(w, http.StatusBadRequest, `body must be empty or {"awake": [session ids]}`)
		return
	}
	f := s.openFlag(r)
	if body.Awake != nil {
		if err := s.mgr.MarkWindowWake(f.WindowID, *body.Awake); err != nil {
			writeError(w, http.StatusServiceUnavailable, "could not save which sessions are awake — the window is still open: "+err.Error())
			return
		}
	}
	last, members, err := s.mgr.SetWindowOpen(f, false)
	if err != nil {
		writeWindowErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"last": last, "members": members})
}

// reviveClearsWake wraps the revive route: a session that woke has no reason
// to be woken by its window's next open, so its mark goes. Only after a
// revive that answered success; a failed clear leaves a mark on a live
// session, which an open skips (it wakes only sessions asleep).
func (s *Server) reviveClearsWake(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		cw := &captureWriter{ResponseWriter: w, status: http.StatusOK}
		next(cw, r)
		if cw.status >= 300 {
			return
		}
		if err := s.mgr.ClearWakeMark(r.PathValue("id")); err != nil {
			log.Printf("windows: revive %s: wake mark not cleared: %v", r.PathValue("id"), err)
		}
	}
}
