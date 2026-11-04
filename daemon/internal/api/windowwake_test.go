package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"ccmux.dev/ccmuxd/internal/manager"
	"ccmux.dev/ccmuxd/internal/model"
	"ccmux.dev/ccmuxd/internal/store"
)

// wakeFixture is a server over a real store (optionally wrapped) with two
// sessions in the CHARTLABS window.
func wakeFixture(t *testing.T, wrap func(store.Store) store.Store) (*Server, string) {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "wake.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	var backing store.Store = st
	if wrap != nil {
		backing = wrap(st)
	}
	s := NewServer(manager.New(context.Background(), nil, backing))
	s.identity = fakeResolver{login: "patric@x.com", ok: true}
	hubWire(s, &model.Workspace{ID: "w1"}, &model.Workspace{ID: "w2"})
	for _, id := range []string{"w1", "w2"} {
		if rec := putGroupReq(t, s, id, "CHARTLABS"); rec.Code != http.StatusNoContent {
			t.Fatal(rec.Code)
		}
	}
	wid, _ := s.mgr.WindowByName("CHARTLABS")
	return s, wid
}

// windowPost posts an open or close as device dev and returns the recorder.
func windowPost(s *Server, wid, verb, dev, body string) *httptest.ResponseRecorder {
	url := "/v1/windows/" + wid + "/" + verb + "?device=" + dev
	req := httptest.NewRequest("POST", url, nil)
	if body != "" {
		req = httptest.NewRequest("POST", url, strings.NewReader(body))
	}
	req.SetPathValue("id", wid)
	rec := httptest.NewRecorder()
	s.setWindowOpen(verb == "open")(rec, req)
	return rec
}

func windowCall(t *testing.T, s *Server, wid, verb, dev, body string) map[string]any {
	t.Helper()
	rec := windowPost(s, wid, verb, dev, body)
	if rec.Code != http.StatusOK {
		t.Fatalf("%s as %s = %d (%s)", verb, dev, rec.Code, rec.Body)
	}
	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v (%s)", err, rec.Body)
	}
	return out
}

func wakeOf(t *testing.T, out map[string]any) []any {
	t.Helper()
	wake, ok := out["wake"].([]any)
	if !ok {
		t.Fatalf("answer %v has no wake list", out)
	}
	return wake
}

// The close's awake list comes back on every open, until the session wakes;
// a close with no body (the Mac) leaves the marks alone; "all asleep" is an
// empty list, not a missing one.
func TestWindows_WakeMarksRoundTrip(t *testing.T) {
	s, wid := wakeFixture(t, nil)
	if wake := wakeOf(t, windowCall(t, s, wid, "open", "web", "")); len(wake) != 0 {
		t.Fatalf("a window never closed wakes %v; want nothing", wake)
	}
	if out := windowCall(t, s, wid, "close", "web", `{"awake":["w1"]}`); out["last"] != true {
		t.Fatalf("close = %v; want last", out)
	}
	for _, dev := range []string{"web", "phone"} { // reading uses nothing up
		if wake := wakeOf(t, windowCall(t, s, wid, "open", dev, "")); !reflect.DeepEqual(wake, []any{"w1"}) {
			t.Fatalf("open as %s wakes %v; want [w1]", dev, wake)
		}
	}
	windowCall(t, s, wid, "close", "phone", "")
	windowCall(t, s, wid, "close", "web", "") // no body: marks stay
	if wake := wakeOf(t, windowCall(t, s, wid, "open", "web", "")); !reflect.DeepEqual(wake, []any{"w1"}) {
		t.Fatalf("after a bare close, open wakes %v; want [w1] kept", wake)
	}
	windowCall(t, s, wid, "close", "web", `{"awake":[]}`)
	if wake := wakeOf(t, windowCall(t, s, wid, "open", "web", "")); len(wake) != 0 {
		t.Fatalf("after an all-asleep close, open wakes %v; want []", wake)
	}
}

// A session that wakes loses its mark; a revive that failed keeps it.
func TestWindows_ReviveClearsTheWakeMark(t *testing.T) {
	s, wid := wakeFixture(t, nil)
	windowCall(t, s, wid, "open", "web", "")
	windowCall(t, s, wid, "close", "web", `{"awake":["w1","w2"]}`)
	revive := func(id string, code int) {
		req := httptest.NewRequest("POST", "/v1/workspaces/"+id+"/revive", nil)
		req.SetPathValue("id", id)
		s.reviveClearsWake(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(code) })(httptest.NewRecorder(), req)
	}
	revive("w1", http.StatusOK)
	revive("w2", http.StatusBadGateway)
	if wake := wakeOf(t, windowCall(t, s, wid, "open", "web", "")); !reflect.DeepEqual(wake, []any{"w2"}) {
		t.Fatalf("after w1 woke and w2 failed, open wakes %v; want [w2]", wake)
	}
}

// brokenMarks fails the wake-mark write and read, the way a locked database
// would, while every other store call works.
type brokenMarks struct{ store.Store }

func (brokenMarks) SetWakeMarks([]string, []string) error { return errors.New("database is locked") }
func (brokenMarks) WakeMarks([]string) ([]string, error) {
	return nil, errors.New("database is locked")
}

// Marks that cannot be saved refuse the close, and marks that cannot be read
// refuse the open: either way the window stays as it was, so trying again
// loses nothing.
func TestWindows_UnwritableMarksRefuseTheCloseAndUnreadableTheOpen(t *testing.T) {
	s, wid := wakeFixture(t, func(st store.Store) store.Store { return brokenMarks{st} })
	if _, _, err := s.mgr.SetWindowOpen(store.WindowOpenFlag{Login: "patric@x.com", WindowID: wid, Device: "web"}, true); err != nil {
		t.Fatal(err)
	}
	if rec := windowPost(s, wid, "close", "web", `{"awake":["w1"]}`); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("close with unwritable marks = %d; want 503", rec.Code)
	}
	if _, opens, _, err := s.mgr.WindowsStrict(); err != nil || len(opens[wid]) != 1 {
		t.Fatalf("after the refused close, opens = %v (%v); want the window still open", opens[wid], err)
	}
	if out := windowCall(t, s, wid, "close", "web", ""); out["last"] != true {
		t.Fatalf("a close with no list does not touch the marks = %v; want last", out)
	}
	if rec := windowPost(s, wid, "open", "web", ""); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("open with unreadable marks = %d; want 503", rec.Code)
	}
	if _, opens, _, _ := s.mgr.WindowsStrict(); len(opens[wid]) != 0 {
		t.Fatalf("after the refused open, opens = %v; want the window still closed", opens[wid])
	}
}

// A close body that is not JSON is refused, not read as "no list".
func TestWindows_CloseRefusesABrokenBody(t *testing.T) {
	s, wid := wakeFixture(t, nil)
	windowCall(t, s, wid, "open", "web", "")
	if rec := windowPost(s, wid, "close", "web", "{nope"); rec.Code != http.StatusBadRequest {
		t.Fatalf("a close with a broken body = %d; want 400", rec.Code)
	}
}
