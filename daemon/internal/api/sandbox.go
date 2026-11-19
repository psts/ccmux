package api

import (
	"errors"
	"net/http"

	"ccmux.dev/ccmuxd/internal/walls"
)

// The Sandbox tab of Settings: the caps every session and job service runs
// under, whether task and thread work is walled, and what this host
// allows. Routes of their own, not fields of /v1/settings: a save here
// rewrites systemd unit files, and the answer carries a probe of the host.

// sandboxService is the daemon's side of the walls (cmd/ccmuxd/sandbox.go).
type sandboxService interface {
	// Host is what this host allows: asked now when fresh, else the last
	// answer.
	Host(fresh bool) walls.Host
	// Apply makes the units' caps what the settings say.
	Apply(walls.Settings) error
}

// SetSandbox turns the sandbox routes on. Without it (macOS, a host with no
// systemd) they answer that there is nothing to wall here.
func (s *Server) SetSandbox(sb sandboxService) { s.sandbox = sb }

// sandboxView is what the routes answer: the settings as saved, the
// defaults (what "reset" means), and the host.
type sandboxView struct {
	Settings walls.Settings `json:"settings"`
	Defaults walls.Settings `json:"defaults"`
	Host     walls.Host     `json:"host"`
}

// sandboxOff answers for a daemon that runs no units, and reports whether
// it did.
func (s *Server) sandboxOff(w http.ResponseWriter) bool {
	if s.sandbox != nil {
		return false
	}
	writeError(w, http.StatusServiceUnavailable, "this daemon runs no session or job services, so there is nothing to cap or wall")
	return true
}

func (s *Server) writeSandbox(w http.ResponseWriter, fresh bool) {
	set, err := s.mgr.Sandbox()
	if err != nil {
		// Never the defaults on a failed read: a page that showed "off" and
		// saved would switch the walls off.
		writeError(w, http.StatusServiceUnavailable, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, sandboxView{Settings: set, Defaults: walls.Defaults(), Host: s.sandbox.Host(fresh)})
}

// getSandbox: GET /v1/sandbox.
func (s *Server) getSandbox(w http.ResponseWriter, _ *http.Request) {
	if s.sandboxOff(w) {
		return
	}
	s.writeSandbox(w, false)
}

// checkSandbox: POST /v1/sandbox/check asks the host again (a person ran
// the fix and wants to see it worked).
func (s *Server) checkSandbox(w http.ResponseWriter, _ *http.Request) {
	if s.sandboxOff(w) {
		return
	}
	s.writeSandbox(w, true)
}

// putSandbox: PUT /v1/sandbox saves the settings whole and applies the
// caps. The walls apply to each unit at its next start; switching them on
// asks the host afresh, so the answer says at once whether it can.
func (s *Server) putSandbox(w http.ResponseWriter, r *http.Request) {
	if s.sandboxOff(w) {
		return
	}
	var set walls.Settings
	if !decodeJSON(w, r, &set) {
		return
	}
	if err := s.mgr.SetSandbox(set); err != nil {
		status := http.StatusInternalServerError
		if errors.Is(err, walls.ErrBadSettings) {
			status = http.StatusBadRequest
		}
		writeError(w, status, err.Error())
		return
	}
	if err := s.sandbox.Apply(set); err != nil {
		writeError(w, http.StatusInternalServerError, "saved, and the units' caps were not updated: "+err.Error())
		return
	}
	s.writeSandbox(w, set.On)
}
