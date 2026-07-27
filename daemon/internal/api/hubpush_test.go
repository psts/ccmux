package api

import (
	"encoding/json"
	"net/http/httptest"
	"testing"

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
