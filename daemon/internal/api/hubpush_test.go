package api

import (
	"encoding/json"
	"net/http/httptest"
	"testing"
	"time"

	"ccmux.dev/ccmuxd/internal/model"
)

// TestFederatedFocus_Union: the hub suppresses pushes for a user focused on the
// hub OR on any member host (so watching a remote session quiets the phone). The
// local oracle returns a fresh copy each call (presenceHub does), so the union
// never mutates shared state.
func TestFederatedFocus_Union(t *testing.T) {
	f := &federatedFocus{
		local:  copyingFocus{"alice": true},
		remote: map[string]bool{"bob": true},
	}
	got := f.ActiveOwners()
	if !got["alice"] || !got["bob"] {
		t.Fatalf("union = %v, want alice + bob", got)
	}
}

// copyingFocus mimics presenceHub.ActiveOwners: a fresh map per call.
type copyingFocus map[string]bool

func (c copyingFocus) DriverLogin(string) (string, int64, bool) { return "", 0, false }

func (c copyingFocus) ActiveOwners() map[string]bool {
	out := map[string]bool{}
	for k, v := range c {
		out[k] = v
	}
	return out
}

func TestAttentionEventFromFrame(t *testing.T) {
	ev, ok := attentionEventFromFrame([]byte(`{"t":"attention","workspace":"w1","pane":"p1","state":"needs_input"}`))
	if !ok || ev.Kind != "attention" || ev.WorkspaceID != "w1" || ev.PaneID != "p1" || ev.Attention != model.AttentionNeedsInput {
		t.Fatalf("event = %+v ok=%v", ev, ok)
	}
	if _, ok := attentionEventFromFrame([]byte(`{"t":"hello","attention":[]}`)); ok {
		t.Error("a hello frame must not convert to an attention event")
	}
	if _, ok := attentionEventFromFrame([]byte(`not json`)); ok {
		t.Error("garbage must not convert")
	}
}

// TestRetainOrExpire: a member's last presence answer survives failed polls only
// within presenceStaleAfter. Within the window a blip must not flicker alerts or
// suppression; past it the member has hibernated or died, and pretending its
// person is still at that screen is what kept phones silent when their Mac slept.
func TestRetainOrExpire(t *testing.T) {
	now := time.Unix(1000000, 0)
	answered := memberOwners{owners: map[string]bool{"alice@example.com": true}, asOf: now}

	fresh := map[string]memberOwners{}
	retainOrExpire(fresh, map[string]memberOwners{"h1": answered}, "h1", now.Add(presenceStaleAfter))
	if !fresh["h1"].owners["alice@example.com"] {
		t.Fatal("an answer at the staleness boundary was dropped; a blip must keep the last answer")
	}

	fresh = map[string]memberOwners{}
	retainOrExpire(fresh, map[string]memberOwners{"h1": answered}, "h1", now.Add(presenceStaleAfter+time.Second))
	if _, kept := fresh["h1"]; kept {
		t.Fatal("a stale answer was retained; a hibernated Mac must stop suppressing its person's pushes")
	}

	fresh = map[string]memberOwners{}
	retainOrExpire(fresh, map[string]memberOwners{}, "h1", now)
	if len(fresh) != 0 {
		t.Fatalf("fresh = %v, want empty when the host never answered", fresh)
	}
}

// TestPresenceOwners: the endpoint the hub polls reports each focused user's login.
func TestPresenceOwners(t *testing.T) {
	s := settingsServer(t)
	s.presence = newPresenceHub(s.mgr)
	s.presence.Join("w1", ClientInfo{Focused: "p1"}, "alice@example.com", "alice@example.com")
	s.presence.Join("w2", ClientInfo{Focused: ""}, "bob@example.com", "bob@example.com") // unfocused → excluded

	rec := httptest.NewRecorder()
	s.presenceOwners(rec, httptest.NewRequest("GET", "/v1/presence", nil))
	var out struct {
		Owners []string `json:"owners"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(out.Owners) != 1 || out.Owners[0] != "alice@example.com" {
		t.Fatalf("owners = %v, want [alice@example.com] (bob is unfocused)", out.Owners)
	}
}
