package api

import (
	"context"
	"encoding/json"
	"testing"

	"ccmux.dev/ccmuxd/internal/manager"
	"ccmux.dev/ccmuxd/internal/model"
)

func newHub(t *testing.T) *presenceHub {
	t.Helper()
	return newPresenceHub(manager.New(context.Background(), nil, nil))
}

// The case this whole split exists for.
//
// A Mac with ccmux open on another Space, or showing a local workspace, has no
// focused hosted pane. Under the old rule that made it indistinguishable from a
// machine nobody was sitting at: no notification here, and a phone buzzing in
// your pocket while you stared at the screen.
func TestActiveOwners_PresentWithoutAFocusedPane(t *testing.T) {
	h := newHub(t)
	id := h.Join("ws1", ClientInfo{User: "dev"}, "dev", "")
	h.SetPresent("ws1", id, true)

	if !h.ActiveOwners()["dev"] {
		t.Fatal("a lens that reports it is at a screen must count, focused pane or not")
	}
}

// The other direction: a locked or sleeping Mac reports present=false and must
// stop counting, so the push actually goes to the phone. This must hold even
// while a pane is still nominally focused, which is exactly the state a screen
// lock leaves behind.
func TestActiveOwners_AbsentEvenWithAFocusedPane(t *testing.T) {
	h := newHub(t)
	id := h.Join("ws1", ClientInfo{User: "dev"}, "dev", "")
	h.Focus("ws1", id, "pane-1")
	h.SetPresent("ws1", id, false)

	if h.ActiveOwners()["dev"] {
		t.Fatal("a lens that says its screen is gone must not keep suppressing pushes")
	}
}

// Compatibility. A lens too old to report presence has to behave exactly as it
// did before this existed, or upgrading the daemon alone would silence every Mac
// in the fleet: absent would read as false and nobody would ever be present.
func TestActiveOwners_OlderLensFallsBackToFocus(t *testing.T) {
	h := newHub(t)
	focused := h.Join("ws1", ClientInfo{User: "old"}, "old", "")
	h.Focus("ws1", focused, "pane-1")
	h.Join("ws1", ClientInfo{User: "idle"}, "idle", "")

	owners := h.ActiveOwners()
	if !owners["old"] {
		t.Error("a pre-presence lens with a focused pane must still count as present")
	}
	if owners["idle"] {
		t.Error("a pre-presence lens with no focused pane must not count")
	}
}

// One device being away must not speak for the person's other devices.
func TestActiveOwners_AnyPresentDeviceCounts(t *testing.T) {
	h := newHub(t)
	mac := h.Join("ws1", ClientInfo{User: "dev", Device: "mac"}, "dev", "")
	phone := h.Join("ws1", ClientInfo{User: "dev", Device: "phone"}, "dev", "")
	h.SetPresent("ws1", mac, true)
	h.SetPresent("ws1", phone, false)

	if !h.ActiveOwners()["dev"] {
		t.Fatal("one present device is enough to say this person is at a screen")
	}
}

// Presence is reported per connection, so a lens that never identifies itself
// cannot be credited to anyone.
func TestActiveOwners_UnidentifiedLensIsIgnored(t *testing.T) {
	h := newHub(t)
	id := h.Join("ws1", ClientInfo{User: "anon"}, "", "")
	h.SetPresent("ws1", id, true)

	if len(h.ActiveOwners()) != 0 {
		t.Fatalf("owners = %v, want empty for a lens with no login", h.ActiveOwners())
	}
}

// Alerting a present lens and pushing to an absent one are the same question read
// two ways, so they must never both fire for one person. Pinned together because
// they live in different files and drifted apart twice before.
func TestPresence_AlertAndPushAreMutuallyExclusive(t *testing.T) {
	for _, present := range []bool{true, false} {
		h := newHub(t)
		id := h.Join("ws1", ClientInfo{User: "dev"}, "dev", "")
		h.SetPresent("ws1", id, present)
		s := &Server{presence: h, focus: h}

		alerted := s.alertsFor("dev", model.AttentionNeedsInput)
		pushSuppressed := h.ActiveOwners()["dev"]
		if alerted != pushSuppressed {
			t.Errorf("present=%v: lens alert=%v but push suppression=%v; one channel must fire, exactly one",
				present, alerted, pushSuppressed)
		}
	}
}

// Re-reporting the same presence must not spam every attached lens with presence
// broadcasts. The Mac calls syncFocusFrames on every wake, poll reconcile, and
// window change.
func TestSetPresent_RepeatIsQuiet(t *testing.T) {
	h := newHub(t)
	id := h.Join("ws1", ClientInfo{User: "dev"}, "dev", "")
	h.SetPresent("ws1", id, true)
	h.SetPresent("ws1", id, true)

	if !h.ActiveOwners()["dev"] {
		t.Fatal("a repeated report must not clear presence")
	}
}

// The wire-level compatibility case, which the unit tests above cannot reach:
// an OLD lens sends {"t":"focus","pane":"..."} with no `present` key at all.
// readLoop must leave presence unreported so atAScreen falls back to focus,
// rather than reading the missing field as false and calling that lens absent.
//
// This is the reason wsMsg.Present is a *bool. A plain bool makes "did not say"
// and "said no" the same value, and every pre-presence Mac in the fleet would go
// silent the moment the daemon upgraded.
func TestReadLoop_FocusWithoutPresenceKeepsTheFallback(t *testing.T) {
	var msg wsMsg
	if err := json.Unmarshal([]byte(`{"t":"focus","pane":"p1"}`), &msg); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if msg.Present != nil {
		t.Fatalf("an omitted present decoded as %v, want nil (nil is what preserves the fallback)", *msg.Present)
	}

	// And the explicit forms must survive the same trip.
	for _, tc := range []struct {
		raw  string
		want bool
	}{
		{`{"t":"focus","pane":"","present":true}`, true},
		{`{"t":"focus","pane":"p1","present":false}`, false},
	} {
		var m wsMsg
		if err := json.Unmarshal([]byte(tc.raw), &m); err != nil {
			t.Fatalf("decode %s: %v", tc.raw, err)
		}
		if m.Present == nil || *m.Present != tc.want {
			t.Errorf("%s decoded present=%v, want %v", tc.raw, m.Present, tc.want)
		}
	}
}

// Full path: a lens that reports present=false while still focused must stop
// counting, and one that never mentions presence must keep counting on focus
// alone. Drives presenceHub the way readLoop does.
func TestPresence_WireShapesDriveTheRightFallback(t *testing.T) {
	h := newHub(t)

	old := h.Join("ws1", ClientInfo{User: "old"}, "old", "")
	h.Focus("ws1", old, "p1") // ...and never a SetPresent, like a pre-presence lens

	modern := h.Join("ws1", ClientInfo{User: "modern"}, "modern", "")
	h.Focus("ws1", modern, "p1")
	h.SetPresent("ws1", modern, false) // screen locked, focus untouched

	owners := h.ActiveOwners()
	if !owners["old"] {
		t.Error("a lens that never reports presence must keep the focus fallback")
	}
	if owners["modern"] {
		t.Error("an explicit present=false must win over a stale focused pane")
	}
}
