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
)

// putWindowSize goes through the real router: the route sits beside
// PUT /v1/windows/{id} (rename), and a pattern mix-up there would send a
// size to the rename handler, which reverts the window's name on the Mac.
func putWindowSize(t *testing.T, s *Server, id, body string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest("PUT", "/v1/windows/"+id+"/size", strings.NewReader(body)))
	return rec
}

type sizedWindow struct {
	ID     string `json:"id"`
	Width  int    `json:"width"`
	Height int    `json:"height"`
}

func listSizes(t *testing.T, s *Server) map[string]sizedWindow {
	t.Helper()
	rec := httptest.NewRecorder()
	s.listWindows(rec, httptest.NewRequest("GET", "/v1/windows", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("list windows = %d (%s)", rec.Code, rec.Body)
	}
	var out []sizedWindow
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode windows: %v (%s)", err, rec.Body)
	}
	byID := map[string]sizedWindow{}
	for _, w := range out {
		byID[w.ID] = w
	}
	return byID
}

// A size one person leaves a window at is what the next person to open it
// sees: it is shared, like the window. A window nobody sized carries none,
// and the Mac falls back to its own default.
func TestWindowSize_SharedAcrossCallers(t *testing.T) {
	s := windowsFixture(t, fakeResolver{login: "dasha@x.com", ok: true},
		&model.Workspace{ID: "w1"}, &model.Workspace{ID: "w2"})
	_ = putGroupReq(t, s, "w1", "DASHA")
	_ = putGroupReq(t, s, "w2", "OTHER")
	dashaID, _ := s.mgr.WindowByName("DASHA")
	otherID, _ := s.mgr.WindowByName("OTHER")

	if rec := putWindowSize(t, s, dashaID, `{"width":1500,"height":900}`); rec.Code != http.StatusNoContent {
		t.Fatalf("set size = %d (%s)", rec.Code, rec.Body)
	}
	s.identity = fakeResolver{login: "patric@x.com", ok: true}
	got := listSizes(t, s)
	if got[dashaID].Width != 1500 || got[dashaID].Height != 900 {
		t.Fatalf("patric sees %+v, want dasha's 1500x900", got[dashaID])
	}
	if got[otherID].Width != 0 || got[otherID].Height != 0 {
		t.Fatalf("an unsized window carries %+v, want none", got[otherID])
	}
}

// A size for a window that does not exist is a 404, a nonsense size a 400;
// neither is stored.
func TestWindowSize_RejectsUnknownWindowAndBadSizes(t *testing.T) {
	s := windowsFixture(t, fakeResolver{login: "patric@x.com", ok: true}, &model.Workspace{ID: "w1"})
	_ = putGroupReq(t, s, "w1", "ALPHA")
	id, _ := s.mgr.WindowByName("ALPHA")

	if rec := putWindowSize(t, s, "no-such-window", `{"width":1200,"height":800}`); rec.Code != http.StatusNotFound {
		t.Fatalf("unknown window = %d, want 404", rec.Code)
	}
	for _, body := range []string{`{"width":0,"height":800}`, `{"width":1200,"height":-5}`, `{"width":30000,"height":800}`, `{}`} {
		if rec := putWindowSize(t, s, id, body); rec.Code != http.StatusBadRequest {
			t.Fatalf("size %s = %d, want 400", body, rec.Code)
		}
	}
	if got := listSizes(t, s)[id]; got.Width != 0 || got.Height != 0 {
		t.Fatalf("a rejected size was stored: %+v", got)
	}
}

type brokenWindowSizes struct{ store.Store }

func (brokenWindowSizes) WindowSizes() (map[string]store.WindowSize, error) {
	return nil, errors.New("database is locked")
}

// Sizes only pick where a window opens. An unreadable size table serves the
// list without them rather than failing it: a 503 here would make both lenses
// keep a stale list over something cosmetic.
func TestWindowSize_UnreadableSizesStillListWindows(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "sizes.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	s := NewServer(manager.New(context.Background(), nil, brokenWindowSizes{Store: st}))
	s.identity = fakeResolver{login: "patric@x.com", ok: true}
	hubWire(s, &model.Workspace{ID: "w1"})
	_ = putGroupReq(t, s, "w1", "ALPHA")
	id, _ := s.mgr.WindowByName("ALPHA")

	if got := listSizes(t, s); got[id].ID != id {
		t.Fatalf("window missing from the list: %v", got)
	}
}
