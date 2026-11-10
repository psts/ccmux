package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"ccmux.dev/ccmuxd/internal/manager"
	"ccmux.dev/ccmuxd/internal/model"
	"ccmux.dev/ccmuxd/internal/store"
	"ccmux.dev/ccmuxd/internal/taskq"
)

func putWindowSlots(t *testing.T, s *Server, id, body string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest("PUT", "/v1/windows/"+id+"/slots", strings.NewReader(body)))
	return rec
}

// listSlots maps window id → taskSlots as served, nil where it is absent.
func listSlots(t *testing.T, s *Server) map[string]*int {
	t.Helper()
	rec := httptest.NewRecorder()
	s.listWindows(rec, httptest.NewRequest("GET", "/v1/windows", nil))
	var out []struct {
		ID        string `json:"id"`
		TaskSlots *int   `json:"taskSlots"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode windows: %v (%s)", err, rec.Body)
	}
	m := map[string]*int{}
	for _, w := range out {
		m[w.ID] = w.TaskSlots
	}
	return m
}

// A window serves the number its queue runs at: its own once set (0 holds
// it), the daemon's default before. The runner reads the same numbers.
func TestWindowSlots_SetServedAndRead(t *testing.T) {
	s := windowsFixture(t, fakeResolver{login: "patric@x.com", ok: true},
		&model.Workspace{ID: "w1"}, &model.Workspace{ID: "w2"})
	s.taskRunner = &taskq.Runner{Slots: 1, WindowSlots: s.mgr.WindowSlots}
	_ = putGroupReq(t, s, "w1", "ALPHA")
	_ = putGroupReq(t, s, "w2", "BETA")
	a, _ := s.mgr.WindowByName("ALPHA")
	b, _ := s.mgr.WindowByName("BETA")

	if rec := putWindowSlots(t, s, a, `{"slots":0}`); rec.Code != http.StatusNoContent {
		t.Fatalf("set slots = %d (%s)", rec.Code, rec.Body)
	}
	got := listSlots(t, s)
	if got[a] == nil || *got[a] != 0 || got[b] == nil || *got[b] != 1 {
		t.Fatalf("served %v %v, want ALPHA 0 and BETA the default 1", got[a], got[b])
	}
	if own, err := s.taskRunner.WindowSlots(); err != nil || len(own) != 1 || own[a] != 0 {
		t.Fatalf("runner reads %v, want only ALPHA at 0", own)
	}
}

// An unknown window is a 404, a number off the picker a 400, and neither is
// stored. With no runner the list carries no number: nothing would honor it.
func TestWindowSlots_RejectsAndNoRunner(t *testing.T) {
	s := windowsFixture(t, fakeResolver{login: "patric@x.com", ok: true}, &model.Workspace{ID: "w1"})
	_ = putGroupReq(t, s, "w1", "ALPHA")
	id, _ := s.mgr.WindowByName("ALPHA")

	if rec := putWindowSlots(t, s, "no-such-window", `{"slots":1}`); rec.Code != http.StatusNotFound {
		t.Fatalf("unknown window = %d, want 404", rec.Code)
	}
	for _, body := range []string{`{"slots":-1}`, `{"slots":4}`, `{}`, `{"slots":"2"}`} {
		if rec := putWindowSlots(t, s, id, body); rec.Code != http.StatusBadRequest {
			t.Fatalf("slots %s = %d, want 400", body, rec.Code)
		}
	}
	if own, _ := s.mgr.WindowSlots(); len(own) != 0 {
		t.Fatalf("a rejected number was stored: %v", own)
	}
	if got := listSlots(t, s); got[id] != nil {
		t.Fatalf("no runner, yet the list serves %d", *got[id])
	}
}

type brokenWindowSlots struct{ store.Store }

func (brokenWindowSlots) WindowSlots() (map[string]int, error) {
	return nil, errors.New("database is locked")
}

// Unreadable numbers serve none, so the lens hides the picker instead of
// showing the default over a paused window; the windows themselves still list.
func TestWindowSlots_UnreadableServesNoNumber(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "slots.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	s := NewServer(manager.New(context.Background(), nil, brokenWindowSlots{Store: st}))
	s.identity = fakeResolver{login: "patric@x.com", ok: true}
	s.taskRunner = &taskq.Runner{Slots: 1, WindowSlots: s.mgr.WindowSlots}
	hubWire(s, &model.Workspace{ID: "w1"})
	_ = putGroupReq(t, s, "w1", "ALPHA")
	id, _ := s.mgr.WindowByName("ALPHA")

	got := listSlots(t, s)
	if n, listed := got[id]; !listed || n != nil {
		t.Fatalf("listed %v with %v, want ALPHA listed without a number", listed, n)
	}
}
