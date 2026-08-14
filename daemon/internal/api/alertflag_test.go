package api

import (
	"context"
	"testing"

	"ccmux.dev/ccmuxd/internal/manager"
	"ccmux.dev/ccmuxd/internal/model"
)

// The daemon owns the alert decision so the lens does not have to. These pin the
// two halves of it: worth alerting on, AND the lens being written to belongs to
// somebody at a screen.
func TestFirehoseFrame_AlertNeedsBothWorthinessAndPresence(t *testing.T) {
	cases := []struct {
		name    string
		att     model.Attention
		present map[string]bool
		want    bool
	}{
		{"needs input, reader is present", model.AttentionNeedsInput, map[string]bool{"dev": true}, true},
		{"needs input, nobody present", model.AttentionNeedsInput, nil, false},
		{"done never alerts, even present", model.AttentionDone, map[string]bool{"dev": true}, false},
		{"idle is ambient", model.AttentionIdle, map[string]bool{"dev": true}, false},
		{"running is ambient", model.AttentionRunning, map[string]bool{"dev": true}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			p := presenceWithFocus(c.present)
			s := &Server{presence: p, focus: p}
			frame := s.firehoseFrame(manager.Event{
				Kind: "attention", WorkspaceID: "ws1", PaneID: "p1", Attention: c.att,
			}, "dev")
			if frame.Alert != c.want {
				t.Errorf("alert = %v, want %v", frame.Alert, c.want)
			}
			if frame.State != c.att || frame.Workspace != "ws1" {
				t.Errorf("frame lost its payload: %+v", frame)
			}
		})
	}
}

// The flag is stamped for the READER, not for the room.
//
// It used to be a single global boolean — "is anybody, anywhere, at a screen" —
// written identically into every subscriber's stream. So a colleague at their
// desk could make a sleeping Mac alert, and the person actually sitting in front
// of ccmux got nothing whenever that global answer happened to be false.
func TestFirehoseFrame_AlertIsPerRecipient(t *testing.T) {
	p := presenceWithFocus(map[string]bool{"here": true})
	s := &Server{presence: p, focus: p}
	ev := manager.Event{Kind: "attention", WorkspaceID: "ws1", PaneID: "p1", Attention: model.AttentionNeedsInput}

	if !s.firehoseFrame(ev, "here").Alert {
		t.Error("the lens whose owner is at a screen must be told to alert")
	}
	if s.firehoseFrame(ev, "away").Alert {
		t.Error("another dev's presence must not raise an alert on an absent dev's lens")
	}
}

// An unidentified lens is never told to alert. It cannot be matched to a presence
// entry, so claiming it is present would be a guess.
func TestFirehoseFrame_AnonymousLensNeverAlerts(t *testing.T) {
	p := presenceWithFocus(map[string]bool{"dev": true})
	s := &Server{presence: p, focus: p}
	frame := s.firehoseFrame(manager.Event{
		Kind: "attention", WorkspaceID: "ws1", PaneID: "p1", Attention: model.AttentionNeedsInput,
	}, "")
	if frame.Alert {
		t.Error("a lens with no identity must not be told to alert")
	}
}

// Non-attention frames carry no alert, and must keep passing through untouched.
func TestFirehoseFrame_OtherKindsAreUnchanged(t *testing.T) {
	s := &Server{presence: presenceWithFocus(map[string]bool{"dev": true})}
	frame := s.firehoseFrame(manager.Event{Kind: "workspace-status", WorkspaceID: "ws1"}, "dev")
	if frame.T != "workspace-status" || frame.Alert {
		t.Errorf("frame = %+v, want workspace-status with no alert", frame)
	}
}

// presenceWithFocus builds a hub whose ActiveOwners is exactly the given logins,
// via the pre-presence signal (a focused pane). It doubles as the compatibility
// case: these clients never report presence, so they must still count.
func presenceWithFocus(logins map[string]bool) *presenceHub {
	h := newPresenceHub(manager.New(context.Background(), nil, nil))
	for login := range logins {
		id := h.Join("ws1", ClientInfo{User: login}, login, "")
		h.Focus("ws1", id, "some-pane")
	}
	return h
}
