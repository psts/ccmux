package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"ccmux.dev/ccmuxd/internal/harness"
	"ccmux.dev/ccmuxd/internal/manager"
	"ccmux.dev/ccmuxd/internal/store"
	"ccmux.dev/ccmuxd/internal/tmux"
)

func readTaskCap(t *testing.T, s *Server) int {
	t.Helper()
	rec := httptest.NewRecorder()
	s.getSettings(rec, httptest.NewRequest("GET", "/v1/settings", nil))
	var got struct {
		Cap *int `json:"taskSlotsTotal"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil || got.Cap == nil {
		t.Fatalf("settings carry no taskSlotsTotal: %v %s", err, rec.Body)
	}
	return *got.Cap
}

func putTaskCap(s *Server, body string) int {
	rec := httptest.NewRecorder()
	s.putSettings(rec, httptest.NewRequest("PUT", "/v1/settings", strings.NewReader(body)))
	return rec.Code
}

// The cap across windows starts at none (0), saves, reads back where the
// runner reads it, refuses numbers off the picker without storing them, and
// a PUT without the field leaves it alone.
func TestSettings_TaskSlotsTotal(t *testing.T) {
	s := settingsServer(t)
	if got := readTaskCap(t, s); got != 0 {
		t.Fatalf("fresh daemon caps at %d, want 0 (none)", got)
	}
	if code := putTaskCap(s, `{"taskSlotsTotal":2}`); code != 200 {
		t.Fatalf("put = %d", code)
	}
	if got, err := s.mgr.TaskSlotsTotal(); got != 2 || err != nil || readTaskCap(t, s) != 2 {
		t.Fatalf("after put: runner reads %d %v, settings %d; want 2", got, err, readTaskCap(t, s))
	}
	for _, body := range []string{`{"taskSlotsTotal":-1}`, `{"taskSlotsTotal":7}`} {
		if code := putTaskCap(s, body); code != 400 {
			t.Fatalf("%s = %d, want 400", body, code)
		}
	}
	if code := putTaskCap(s, `{"owner":""}`); code != 200 || readTaskCap(t, s) != 2 {
		t.Fatalf("an unrelated put changed the cap to %d", readTaskCap(t, s))
	}
	if code := putTaskCap(s, `{"taskSlotsTotal":0}`); code != 200 || readTaskCap(t, s) != 0 {
		t.Fatalf("clearing the cap left %d", readTaskCap(t, s))
	}
}

// brokenTaskCap is a store whose cap across windows cannot be read; every
// other setting reads as it is.
type brokenTaskCap struct{ store.Store }

func (b brokenTaskCap) GetSetting(key string) (string, error) {
	if key == "task_slots_total" {
		return "", errors.New("database is locked")
	}
	return b.Store.GetSetting(key)
}

// A cap that cannot be read, or that holds no number, is an error all the
// way out: the runner then starts nothing new, and the Settings page gets
// no number at all. A 0 guessed from a failed read would be "no cap" to
// the runner and "No limit" on the page, and saving the page would write
// it. Other settings are not touched by it.
func TestSettings_AnUnreadableTaskCapIsAnError(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "reg.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	serverOn := func(s store.Store) *Server {
		mgr := manager.New(context.Background(), &tmux.Server{Socket: "unused"}, s)
		mgr.Harnesses = harness.New(st)
		return &Server{mgr: mgr}
	}
	get := func(s *Server) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		s.getSettings(rec, httptest.NewRequest("GET", "/v1/settings", nil))
		return rec
	}

	broken := serverOn(brokenTaskCap{Store: st})
	if n, err := broken.mgr.TaskSlotsTotal(); err == nil {
		t.Fatalf("an unreadable cap read as %d with no error", n)
	}
	if rec := get(broken); rec.Code != 503 || strings.Contains(rec.Body.String(), "taskSlotsTotal") {
		t.Fatalf("settings with an unreadable cap = %d %s, want 503 and no number", rec.Code, rec.Body)
	}

	if err := st.SetSetting("task_slots_total", "lots"); err != nil {
		t.Fatal(err)
	}
	garbled := serverOn(st)
	if n, err := garbled.mgr.TaskSlotsTotal(); err == nil || !strings.Contains(err.Error(), "lots") {
		t.Fatalf("a cap that is no number read as %d %v, want an error naming it", n, err)
	}
	if rec := get(garbled); rec.Code != 503 {
		t.Fatalf("settings with a cap that is no number = %d, want 503", rec.Code)
	}

	// A readable cap on the same store serves as before.
	if err := st.SetSetting("task_slots_total", "3"); err != nil {
		t.Fatal(err)
	}
	if got := readTaskCap(t, serverOn(st)); got != 3 {
		t.Fatalf("a readable cap = %d, want 3", got)
	}
}
