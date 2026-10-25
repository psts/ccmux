package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"

	"ccmux.dev/ccmuxd/internal/model"
	"ccmux.dev/ccmuxd/internal/peers"
)

// pageFixture is a window agent on a daemon with push wired to a fake
// sender over the real store, and one subscribed device.
func pageFixture(t *testing.T, subscribed bool) (*windowAgentFixture, *fakeSender, string, string) {
	t.Helper()
	f := newWindowAgentFixture(t, "sleep 8;:")
	f.srv.EnablePeers(peers.NewService(f.st, f.srv.mgr, testSecret))
	sender := &fakeSender{}
	f.srv.sender, f.srv.pushStore, f.srv.pages = sender, f.st, f.st
	if subscribed {
		if err := f.st.SavePushSubscription(&model.PushSubscription{ID: "sub1", Login: "patric", Address: `{"endpoint":"https://push.example/1"}`}); err != nil {
			t.Fatal(err)
		}
	}
	code, pane := f.start(t, "x-poster", "")
	if code != 201 {
		t.Fatalf("start = %d", code)
	}
	_, tok := registerOnPane(t, f, pane.ID, "/agents/x-poster")
	return f, sender, pane.ID, tok
}

func postPage(t *testing.T, f *windowAgentFixture, paneID, tok, incident, message string) (int, string) {
	t.Helper()
	resp := postJSON(t, f.base+"/v1/panes/"+paneID+"/page", tok, map[string]any{"incident": incident, "message": message})
	defer resp.Body.Close()
	var body map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&body)
	out, _ := json.Marshal(body)
	return resp.StatusCode, string(out)
}

func TestPanePage_OncePerIncident(t *testing.T) {
	f, sender, paneID, tok := pageFixture(t, true)
	if code, body := postPage(t, f, paneID, tok, "stripe-down", "Stripe webhooks fail since 10:02"); code != http.StatusOK || !strings.Contains(body, `"sent":1`) {
		t.Fatalf("first page = %d %s", code, body)
	}
	if code, body := postPage(t, f, paneID, tok, "stripe-down", "still down"); code != http.StatusConflict || !strings.Contains(body, "already paged by x-poster") {
		t.Fatalf("repeat = %d %s, want 409 naming the first page", code, body)
	}
	if code, _ := postPage(t, f, paneID, tok, "cve-2026-1", "new CVE in backend"); code != http.StatusOK {
		t.Fatalf("a new incident = %d, want a page", code)
	}
	if len(sender.sent) != 2 || sender.sent[0].topic == sender.sent[1].topic || strings.Contains(sender.sent[0].topic, "-") {
		t.Fatalf("sent = %+v, want two pages with their own topics", sender.sent)
	}
}

func TestPanePage_RefusalsAndNobodyToPage(t *testing.T) {
	f, _, paneID, tok := pageFixture(t, false)
	if code, _ := postPage(t, f, paneID, "", "x", "y"); code != http.StatusUnauthorized {
		t.Errorf("no token = %d, want 401", code)
	}
	for _, bad := range [][2]string{{"", "msg"}, {"key", " "}, {strings.Repeat("k", 81), "msg"}} {
		if code, _ := postPage(t, f, paneID, tok, bad[0], bad[1]); code != http.StatusBadRequest {
			t.Errorf("incident %q message %q = %d, want 400", bad[0], bad[1], code)
		}
	}
	// Nobody subscribed: nothing sent, and the claim is given back.
	if code, _ := postPage(t, f, paneID, tok, "outage", "site down"); code != http.StatusServiceUnavailable {
		t.Fatalf("no device = %d, want 503", code)
	}
	if err := f.st.SavePushSubscription(&model.PushSubscription{ID: "sub1", Login: "patric", Address: `{"endpoint":"https://push.example/1"}`}); err != nil {
		t.Fatal(err)
	}
	if code, body := postPage(t, f, paneID, tok, "outage", "site down"); code != http.StatusOK {
		t.Fatalf("after subscribing = %d %s, want the page to go out", code, body)
	}
}

// A page goes to the workspace's own audience only, even while that human
// sits at a focused lens; another login's devices never get it.
func TestPanePage_OnlyTheAudienceAndDespiteFocus(t *testing.T) {
	f, sender, paneID, tok := pageFixture(t, true) // sub1 is patric's
	if err := f.st.SavePushSubscription(&model.PushSubscription{ID: "sub2", Login: "dasha", Address: `{"endpoint":"https://push.example/2"}`}); err != nil {
		t.Fatal(err)
	}
	f.srv.focus = routedFocus{owners: map[string]bool{"patric": true}, driver: "patric", driving: true}
	if code, body := postPage(t, f, paneID, tok, "leak", "API key in a public commit"); code != http.StatusOK || !strings.Contains(body, `"sent":1`) {
		t.Fatalf("page = %d %s", code, body)
	}
	if len(sender.sent) != 1 || !strings.Contains(sender.sent[0].address, "push.example/1") {
		t.Fatalf("sent = %+v, want patric's device alone", sender.sent)
	}
}

// A push the service answered but did not take is not a page: the claim is
// given back, so the retry pages for real.
func TestPanePage_RejectedPushIsNotSent(t *testing.T) {
	f, sender, paneID, tok := pageFixture(t, true)
	sender.status = map[string]int{`{"endpoint":"https://push.example/1"}`: http.StatusTooManyRequests}
	if code, body := postPage(t, f, paneID, tok, "outage", "site down"); code != http.StatusServiceUnavailable {
		t.Fatalf("rejected push = %d %s, want 503", code, body)
	}
	sender.status = nil
	if code, body := postPage(t, f, paneID, tok, "outage", "site down"); code != http.StatusOK {
		t.Fatalf("retry = %d %s, want the page to go out", code, body)
	}
}

// failingRelease is the real page store whose release always fails.
type failingRelease struct{ pageStore }

func (failingRelease) ReleasePage(string, string) error { return errors.New("database is locked") }

// A release that failed is said, since a retry with the same key is now refused.
func TestPanePage_FailedReleaseIsReported(t *testing.T) {
	f, _, paneID, tok := pageFixture(t, false)
	f.srv.pages = failingRelease{f.st}
	code, body := postPage(t, f, paneID, tok, "outage", "site down")
	if code != http.StatusServiceUnavailable || !strings.Contains(body, "still marked paged") || !strings.Contains(body, "database is locked") {
		t.Fatalf("page = %d %s, want the kept claim reported", code, body)
	}
}
