package api

import (
	"log"
	"net/http"

	"ccmux.dev/ccmuxd/internal/agent"
	"ccmux.dev/ccmuxd/internal/loadout"
	"ccmux.dev/ccmuxd/internal/store"
)

// Skills and plugins per group and repo (internal/loadout): what a thread
// session loads from the capability catalog. A group (a shared window) has
// each entry on or off; a repo (a workspace) has it "as the group", on or
// off. Like the windows themselves, the choices live on the daemon lenses
// talk to and are never proxied. A change applies when a session next
// starts: a running session whose list has moved on is served with
// loadoutStale (sessionView), and nothing restarts by itself.
//
//	GET /v1/capabilities/choices            {windows: {id: [on ids]}, workspaces: {id: {cap: on|off}}}
//	GET /v1/windows/{id}/capabilities       {on: [ids], missing: [ids]}
//	PUT /v1/windows/{id}/capabilities/{cap} {state: "on" | "off"}
//	GET /v1/workspaces/{id}/capabilities    {window, group: [ids], choices: {cap: on|off}, effective: [ids], missing: [ids]}
//	PUT /v1/workspaces/{id}/capabilities/{cap} {state: "group" | "on" | "off"}
//
// missing names ids that are on but no longer in the catalog: a session
// skips them and says so in its loadout's notes.

// choiceStore is what the routes write and check against.
type choiceStore interface {
	SetCapabilityChoice(store.CapChoice) error
	AllWindows() (map[string]string, error)
}

// choiceGroup is the state a repo sends for "as the group": no row.
const choiceGroup = "group"

// SetLoadouts turns the choice routes on, and gives the session routes
// what a session started with and whether it is out of date.
func (s *Server) SetLoadouts(svc *loadout.Service, st choiceStore) { s.loadouts, s.choices = svc, st }

func (s *Server) choiceRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /v1/capabilities/choices", s.listChoices)
	mux.HandleFunc("GET /v1/windows/{id}/capabilities", s.getWindowChoices)
	mux.HandleFunc("PUT /v1/windows/{id}/capabilities/{cap}", s.putWindowChoice)
	mux.HandleFunc("GET /v1/workspaces/{id}/capabilities", s.getWorkspaceChoices)
	mux.HandleFunc("PUT /v1/workspaces/{id}/capabilities/{cap}", s.putWorkspaceChoice)
}

// choiceSnapshot is the choices and the catalog as they are now, or an
// answer already written (503 when loadouts are off, 500 when unreadable).
func (s *Server) choiceSnapshot(w http.ResponseWriter) *loadout.Snapshot {
	if s.loadouts == nil || s.choices == nil {
		writeError(w, http.StatusServiceUnavailable, "skills and plugins choices are off on this host (they need session runners)")
		return nil
	}
	snap, err := s.loadouts.Snapshot()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return nil
	}
	return snap
}

func (s *Server) listChoices(w http.ResponseWriter, r *http.Request) {
	snap := s.choiceSnapshot(w)
	if snap == nil {
		return
	}
	windows, workspaces := snap.All()
	writeJSON(w, http.StatusOK, map[string]any{"windows": windows, "workspaces": workspaces})
}

// windowKnown answers 404 for a window that is not there, 503 when the
// windows cannot be read, and says whether the caller may go on.
func (s *Server) windowKnown(w http.ResponseWriter, id string) bool {
	windows, err := s.choices.AllWindows()
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, err.Error())
		return false
	}
	if _, ok := windows[id]; !ok {
		writeError(w, http.StatusNotFound, "unknown window "+id)
		return false
	}
	return true
}

func (s *Server) getWindowChoices(w http.ResponseWriter, r *http.Request) {
	snap := s.choiceSnapshot(w)
	id := r.PathValue("id")
	if snap == nil || !s.windowKnown(w, id) {
		return
	}
	on := loadout.Effective(snap.Group(id), nil)
	writeJSON(w, http.StatusOK, map[string]any{"on": on, "missing": missingFrom(snap, on)})
}

func missingFrom(snap *loadout.Snapshot, ids []string) []string {
	out := []string{}
	for _, id := range ids {
		if !snap.Has(id) {
			out = append(out, id)
		}
	}
	return out
}

func (s *Server) putWindowChoice(w http.ResponseWriter, r *http.Request) {
	snap := s.choiceSnapshot(w)
	id, capID := r.PathValue("id"), r.PathValue("cap")
	if snap == nil || !s.windowKnown(w, id) {
		return
	}
	state, ok := s.choiceState(w, r, snap, capID, map[string]string{store.ChoiceOn: store.ChoiceOn, store.ChoiceOff: ""})
	if !ok {
		return
	}
	if err := s.choices.SetCapabilityChoice(store.CapChoice{Scope: store.ChoiceWindow, ScopeID: id, CapID: capID, State: state}); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.loadoutsChanged(snap.Members(id)...)
	w.WriteHeader(http.StatusNoContent)
}

// choiceState reads {state} and maps it through allowed onto what is
// stored. Switching on an id the catalog does not hold is refused (404):
// off and "as the group" are always taken, so a choice of something since
// deleted can be cleared.
func (s *Server) choiceState(w http.ResponseWriter, r *http.Request, snap *loadout.Snapshot, capID string, allowed map[string]string) (string, bool) {
	var in struct{ State string }
	if !decodeJSON(w, r, &in) {
		return "", false
	}
	state, ok := allowed[in.State]
	if !ok {
		writeError(w, http.StatusBadRequest, "state is one of: "+stateNames(allowed))
		return "", false
	}
	if state == store.ChoiceOn && !snap.Has(capID) {
		writeError(w, http.StatusNotFound, capID+" is not in the catalog")
		return "", false
	}
	return state, true
}

func stateNames(allowed map[string]string) string {
	names := ""
	for _, name := range []string{choiceGroup, store.ChoiceOn, store.ChoiceOff} {
		if _, ok := allowed[name]; ok {
			if names != "" {
				names += ", "
			}
			names += name
		}
	}
	return names
}

// repoKnown answers 404 for a workspace that is not a repo session this
// daemon serves, and says whether the caller may go on.
func (s *Server) repoKnown(w http.ResponseWriter, r *http.Request, id string) bool {
	if !s.workspaceKnown(r.Context(), id) {
		writeError(w, http.StatusNotFound, "unknown workspace "+id)
		return false
	}
	if ws := s.mgr.Workspace(id); ws != nil && ws.Agent != "" {
		writeError(w, http.StatusBadRequest, id+" is an agent's session: an agent loads what its base names")
		return false
	}
	return true
}

func (s *Server) getWorkspaceChoices(w http.ResponseWriter, r *http.Request) {
	snap := s.choiceSnapshot(w)
	id := r.PathValue("id")
	if snap == nil || !s.repoKnown(w, r, id) {
		return
	}
	window := snap.Window(id)
	writeJSON(w, http.StatusOK, map[string]any{
		"window": window, "group": loadout.Effective(snap.Group(window), nil), "choices": snap.Repo(id),
		"effective": snap.IDs(id), "missing": snap.Missing(id),
	})
}

func (s *Server) putWorkspaceChoice(w http.ResponseWriter, r *http.Request) {
	snap := s.choiceSnapshot(w)
	id, capID := r.PathValue("id"), r.PathValue("cap")
	if snap == nil || !s.repoKnown(w, r, id) {
		return
	}
	state, ok := s.choiceState(w, r, snap, capID, map[string]string{choiceGroup: "", store.ChoiceOn: store.ChoiceOn, store.ChoiceOff: store.ChoiceOff})
	if !ok {
		return
	}
	if err := s.choices.SetCapabilityChoice(store.CapChoice{Scope: store.ChoiceWorkspace, ScopeID: id, CapID: capID, State: state}); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.loadoutsChanged(id)
	w.WriteHeader(http.StatusNoContent)
}

// loadoutsChanged tells every lens that what these workspaces' sessions
// should load changed, so a page listing them reads the list again and
// shows which are now out of date.
func (s *Server) loadoutsChanged(workspaceIDs ...string) {
	for _, id := range workspaceIDs {
		s.mgr.SessionsChanged(id)
	}
}

// sessionLoadouts is the choices and the catalog read once for a list of
// session views; nil when loadouts are off or could not be read, and then
// the views say nothing of loadouts rather than something wrong.
func (s *Server) sessionLoadouts() *loadout.Snapshot {
	if s.loadouts == nil {
		return nil
	}
	snap, err := s.loadouts.Snapshot()
	if err != nil {
		log.Printf("sessions: loadouts not read, served without them: %v", err)
		return nil
	}
	return snap
}

// loadoutOf is what a session started with and whether it is out of date.
func loadoutOf(snap *loadout.Snapshot, sess store.RunnerSession) (*agent.LoadoutRecord, bool) {
	if snap == nil {
		return nil, false
	}
	stale := snap.Stale(sess)
	rec, ok := loadout.Record(sess)
	if !ok {
		return nil, stale
	}
	return &rec, stale
}
