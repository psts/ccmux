package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"ccmux.dev/ccmuxd/internal/model"
	"ccmux.dev/ccmuxd/internal/taskq"
)

func putWindowPriority(t *testing.T, s *Server, id, body string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest("PUT", "/v1/windows/"+id+"/priority", strings.NewReader(body)))
	return rec
}

// listPriorities maps window id → taskPriority as served, nil where absent.
func listPriorities(t *testing.T, s *Server) map[string]*int {
	t.Helper()
	rec := httptest.NewRecorder()
	s.listWindows(rec, httptest.NewRequest("GET", "/v1/windows", nil))
	var out []struct {
		ID           string `json:"id"`
		TaskPriority *int   `json:"taskPriority"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode windows: %v (%s)", err, rec.Body)
	}
	m := map[string]*int{}
	for _, w := range out {
		m[w.ID] = w.TaskPriority
	}
	return m
}

// A priority set is served and read by the runner; 0 clears it, and a
// window with none serves no number.
func TestWindowPriority_SetServedClearedAndRead(t *testing.T) {
	s := windowsFixture(t, fakeResolver{login: "patric@x.com", ok: true},
		&model.Workspace{ID: "w1"}, &model.Workspace{ID: "w2"})
	s.taskRunner = &taskq.Runner{Slots: 1, WindowPriorities: s.mgr.WindowPriorities}
	_ = putGroupReq(t, s, "w1", "ALPHA")
	_ = putGroupReq(t, s, "w2", "BETA")
	a, _ := s.mgr.WindowByName("ALPHA")
	b, _ := s.mgr.WindowByName("BETA")

	for _, body := range []string{`{"priority":4}`, `{"priority":1}`} {
		if rec := putWindowPriority(t, s, a, body); rec.Code != http.StatusNoContent {
			t.Fatalf("set %s = %d (%s)", body, rec.Code, rec.Body)
		}
	}
	got := listPriorities(t, s)
	if got[a] == nil || *got[a] != 1 || got[b] != nil {
		t.Fatalf("served %v %v, want ALPHA 1 and BETA none", got[a], got[b])
	}
	if own, err := s.taskRunner.WindowPriorities(); err != nil || len(own) != 1 || own[a] != 1 {
		t.Fatalf("runner reads %v, want only ALPHA at 1", own)
	}
	if rec := putWindowPriority(t, s, a, `{"priority":0}`); rec.Code != http.StatusNoContent {
		t.Fatalf("clear = %d (%s)", rec.Code, rec.Body)
	}
	if got := listPriorities(t, s); got[a] != nil {
		t.Fatalf("cleared, yet served %d", *got[a])
	}
}

// An unknown window is a 404, a number off the picker or no number a 400,
// and neither is stored. With no runner the list carries no number.
func TestWindowPriority_RejectsAndNoRunner(t *testing.T) {
	s := windowsFixture(t, fakeResolver{login: "patric@x.com", ok: true}, &model.Workspace{ID: "w1"})
	_ = putGroupReq(t, s, "w1", "ALPHA")
	id, _ := s.mgr.WindowByName("ALPHA")

	if rec := putWindowPriority(t, s, "no-such-window", `{"priority":1}`); rec.Code != http.StatusNotFound {
		t.Fatalf("unknown window = %d, want 404", rec.Code)
	}
	for _, body := range []string{`{"priority":-1}`, `{"priority":10}`, `{}`, `{"priority":null}`, `{"priority":"2"}`} {
		if rec := putWindowPriority(t, s, id, body); rec.Code != http.StatusBadRequest {
			t.Fatalf("priority %s = %d, want 400", body, rec.Code)
		}
	}
	if own, _ := s.mgr.WindowPriorities(); len(own) != 0 {
		t.Fatalf("a rejected number was stored: %v", own)
	}
	if rec := putWindowPriority(t, s, id, `{"priority":3}`); rec.Code != http.StatusNoContent {
		t.Fatalf("set = %d", rec.Code)
	}
	if got := listPriorities(t, s); got[id] != nil {
		t.Fatalf("no runner, yet the list serves %d", *got[id])
	}
}
