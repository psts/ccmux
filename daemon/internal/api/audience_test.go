package api

import (
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"ccmux.dev/ccmuxd/internal/model"
)

// routedFocus is a focusOracle with a scripted driver, for audience tests —
// presenceHub sets lastInput to time.Now() on Input, so staleness cannot be
// produced through the real hub.
type routedFocus struct {
	owners   map[string]bool
	driver   string
	atMillis int64
	driving  bool
}

func (f routedFocus) ActiveOwners() map[string]bool { return f.owners }
func (f routedFocus) DriverLogin(string) (string, int64, bool) {
	return f.driver, f.atMillis, f.driving
}

// The audience ladder: recent driver alone; else the window's open-holders;
// else unbounded (everyone — the pre-multi-user behavior).
func TestAlertAudience_Ladder(t *testing.T) {
	ws := &model.Workspace{ID: "w1"}
	s := windowsFixture(t, fakeResolver{login: "patric@x.com", ok: true}, ws)
	if rec := putGroupReq(t, s, "w1", "CHARTLABS"); rec.Code != 204 {
		t.Fatal(rec.Code)
	}
	wid, _ := s.mgr.WindowByName("CHARTLABS")
	if _, _, err := s.mgr.SetWindowOpen("patric@x.com", wid, true); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UnixMilli()

	// Fresh driver → theirs alone, even though patric holds the window open.
	s.focus = routedFocus{driver: "dasha@x.com", atMillis: now, driving: true}
	audience, bounded := s.alertAudience("w1")
	if !bounded || !audience["dasha@x.com"] || audience["patric@x.com"] {
		t.Fatalf("fresh driver: audience = %v bounded=%v, want dasha alone", audience, bounded)
	}

	// Stale driver → falls to the window's open-holders.
	stale := time.Now().Add(-driverRecency - time.Minute).UnixMilli()
	s.focus = routedFocus{driver: "dasha@x.com", atMillis: stale, driving: true}
	audience, bounded = s.alertAudience("w1")
	if !bounded || !audience["patric@x.com"] || audience["dasha@x.com"] {
		t.Fatalf("stale driver: audience = %v bounded=%v, want the window holder", audience, bounded)
	}

	// No driver, window open by nobody → unbounded (nothing goes silent).
	if _, _, err := s.mgr.SetWindowOpen("patric@x.com", wid, false); err != nil {
		t.Fatal(err)
	}
	s.focus = routedFocus{}
	if _, bounded = s.alertAudience("w1"); bounded {
		t.Fatal("window open by nobody must be unbounded, not silence")
	}

	// Ungrouped workspace → unbounded.
	if _, bounded = s.alertAudience("nowhere"); bounded {
		t.Fatal("an ungrouped workspace must be unbounded")
	}
}

// The original complaint, end to end: two people present, the repo in
// patric's open window — only patric's lens is told to alert. Dasha's lens
// stays quiet WITHOUT an identity-mismatch note (deliberate routing is not a
// fault), and an unidentified (old-app) reader keeps the old global rule.
func TestAlertsFor_RoutesByAudience(t *testing.T) {
	ws := &model.Workspace{ID: "w1"}
	s := windowsFixture(t, fakeResolver{login: "patric@x.com", ok: true}, ws)
	if rec := putGroupReq(t, s, "w1", "CHARTLABS"); rec.Code != 204 {
		t.Fatal(rec.Code)
	}
	wid, _ := s.mgr.WindowByName("CHARTLABS")
	if _, _, err := s.mgr.SetWindowOpen("patric@x.com", wid, true); err != nil {
		t.Fatal(err)
	}
	s.focus = routedFocus{owners: map[string]bool{"patric@x.com": true, "dasha@x.com": true}}

	patric := firehoseReader{login: "patric@x.com", identified: true}
	dasha := firehoseReader{login: "dasha@x.com", identified: true}
	if !s.alertsFor(patric, "w1", model.AttentionNeedsInput) {
		t.Error("the window holder must be alerted")
	}
	if s.alertsFor(dasha, "w1", model.AttentionNeedsInput) {
		t.Error("a present reader without the window open must NOT be alerted")
	}
	if len(s.alertMissed) != 0 {
		t.Errorf("routing quiet must not be reported as an identity mismatch: %v", s.alertMissed)
	}
	if !s.alertsFor(firehoseReader{}, "w1", model.AttentionNeedsInput) {
		t.Error("an unidentified reader keeps the old anybody-present rule")
	}
}

// The loopback owner tier vouches the reader, so a Mac on the hub box gets
// per-reader routing instead of the everything-alerts fallback.
func TestReaderOf_OwnerTierIsIdentified(t *testing.T) {
	s := newIdentityServer(t, fakeResolver{ok: false})
	if err := s.mgr.SetOwner("patric@x.com"); err != nil {
		t.Fatal(err)
	}
	id := s.resolveIdentity(req("127.0.0.1:5000", ""))
	if r := readerOf(id); !r.identified || r.login != "patric@x.com" {
		t.Fatalf("owner-tier reader = %+v, want identified as the owner", r)
	}
	// Self-declared stays unidentified: nothing vouched for the name.
	id = s.resolveIdentity(req("100.64.0.9:5000", "?user=someone"))
	if readerOf(id).identified {
		t.Fatal("a self-declared name must not be treated as identified")
	}
}

// The member half of driver federation: the endpoint serves canonical logins
// with their last-typed stamp, and the anon guard keeps unroutable names out.
func TestPresenceDrivers_Endpoint(t *testing.T) {
	s := newIdentityServer(t, fakeResolver{ok: false})
	conn := s.presence.Join("w1", ClientInfo{User: "Patric"}, "patric@x.com", "")
	s.presence.Input("w1", conn)
	anon := s.presence.Join("w2", ClientInfo{User: "anon"}, "anon", "")
	s.presence.Input("w2", anon)

	rec := httptest.NewRecorder()
	s.presenceDrivers(rec, httptest.NewRequest("GET", "/v1/presence/drivers", nil))
	if rec.Code != 200 {
		t.Fatalf("drivers = %d", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, `"w1"`) || !strings.Contains(body, "patric@x.com") {
		t.Fatalf("drivers missing the real driver: %s", body)
	}
	if strings.Contains(body, `"w2"`) {
		t.Fatalf("anon driver must not be served (it would route notifications to nobody real): %s", body)
	}
}
