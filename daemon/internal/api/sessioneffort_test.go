package api

import (
	"net/http"
	"slices"
	"strings"
	"testing"

	"ccmux.dev/ccmuxd/internal/store"
)

// An effort is saved on the row for every later start, and a running
// session's runner takes it at once; one whose runner does not take it
// (a runner from before the route) gets it at its next start.
func TestAnEffortIsSavedAndReachesARunningSession(t *testing.T) {
	r := newStreamRig(t)
	w := callSessions(t, r.srv, "POST", "/v1/sessions/s1/effort", `{"effort":"xhigh"}`)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"applies":"now"`) {
		t.Fatalf("running: %d %s", w.Code, w.Body)
	}
	if w := callSessions(t, r.srv, "POST", "/v1/sessions/s1/effort", `{"effort":""}`); w.Code != http.StatusOK {
		t.Fatalf("back to the model's own: %d %s", w.Code, w.Body)
	}
	if w := callSessions(t, r.srv, "POST", "/v1/sessions/s1/effort", `{"effort":"huge"}`); w.Code != http.StatusBadRequest {
		t.Fatalf("only a listed effort is taken: %d", w.Code)
	}
	r.proc.mu.Lock()
	r.proc.down = true
	r.proc.mu.Unlock()
	w = callSessions(t, r.srv, "POST", "/v1/sessions/s1/effort", `{"effort":"low"}`)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "on the next restart") {
		t.Fatalf("a runner that did not take it: saved for the next start: %d %s", w.Code, w.Body)
	}
	r.sup.set(store.SessionAsleep)
	w = callSessions(t, r.srv, "POST", "/v1/sessions/s1/effort", `{"effort":"max"}`)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "when it next starts") {
		t.Fatalf("asleep: saved for its next start: %d %s", w.Code, w.Body)
	}
	if !slices.Equal(r.sup.efforts, []string{"xhigh", "", "low", "max"}) {
		t.Fatalf("saved: %q", r.sup.efforts)
	}
	if !slices.Equal(r.proc.did, []string{"effort xhigh", "effort "}) {
		t.Fatalf("what the runner was asked: %v", r.proc.did)
	}
}

func TestAnAgentsEffortIsItsBases(t *testing.T) {
	r := newStreamRig(t)
	r.rows.setKind(store.SessionAgent)
	if w := callSessions(t, r.srv, "POST", "/v1/sessions/s1/effort", `{"effort":"high"}`); w.Code != http.StatusConflict {
		t.Fatalf("an agent: %d %s", w.Code, w.Body)
	}
	if len(r.sup.efforts) != 0 {
		t.Fatalf("saved for an agent: %q", r.sup.efforts)
	}
}
