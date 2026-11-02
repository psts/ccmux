package api

import (
	"encoding/json"
	"net/http/httptest"
	"testing"

	"ccmux.dev/ccmuxd/internal/model"
	"ccmux.dev/ccmuxd/internal/version"
)

func TestLastDriven(t *testing.T) {
	list := []*model.Workspace{{ID: "a"}, {ID: "b"}, {ID: "c"}, {ID: "d"}}
	drivers := map[string]struct {
		login string
		at    int64
	}{
		"a": {"patric@x.com", 100},
		"b": {"patric@x.com", 300}, // the newest of his
		"c": {"dasha@x.com", 900},  // newer, but someone else's
	}
	driver := func(id string) (string, int64, bool) {
		d, ok := drivers[id]
		return d.login, d.at, ok
	}
	if ws, at := lastDriven(list, "patric@x.com", driver); ws != "b" || at != 300 {
		t.Fatalf("patric: %s at %d, want b at 300", ws, at)
	}
	if ws, _ := lastDriven(list, "nobody@x.com", driver); ws != "" {
		t.Fatalf("a login that typed nowhere got %q", ws)
	}
	if ws, _ := lastDriven(list, "anon", driver); ws != "" {
		t.Fatalf("anon got %q: anonymous typists are not one person", ws)
	}
}

// driverFocus is a focusOracle with fixed drivers.
type driverFocus map[string]struct {
	login string
	at    int64
}

func (d driverFocus) ActiveOwners() map[string]bool { return nil }
func (d driverFocus) DriverLogin(id string) (string, int64, bool) {
	v, ok := d[id]
	return v.login, v.at, ok
}

// On a hub the last session can live on a member host: "wr" is remote and was
// typed in last, "wl" is the hub's own and older. A hub that searched only its
// own sessions answered "wl", and a phone opened on the wrong one.
func TestLastSession_HubCountsMemberSessions(t *testing.T) {
	h, _ := hubFixture(t, version.Contract)
	s := newIdentityServer(t, fakeResolver{ok: false})
	s.hub = h
	s.focus = driverFocus{"wl": {"patric", 100}, "wr": {"patric", 500}}

	rec := httptest.NewRecorder()
	s.lastSession(rec, httptest.NewRequest("GET", "/v1/me/last-session?user=patric", nil))
	var got struct {
		WorkspaceID string `json:"workspaceId"`
		At          int64  `json:"at"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("body %q: %v", rec.Body.String(), err)
	}
	if got.WorkspaceID != "wr" || got.At != 500 {
		t.Fatalf("last session = %+v, want wr at 500 (the member host's)", got)
	}
}
