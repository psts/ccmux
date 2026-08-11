package api

import (
	"context"
	"testing"

	"ccmux.dev/ccmuxd/internal/manager"
	"ccmux.dev/ccmuxd/internal/model"
)

// The daemon owns the alert decision so the lens does not have to. These pin the
// two halves of it: worth alerting on, AND somebody at a screen to see it.
func TestFirehoseFrame_AlertNeedsBothWorthinessAndPresence(t *testing.T) {
	cases := []struct {
		name    string
		att     model.Attention
		focused map[string]bool
		want    bool
	}{
		{"needs input, someone present", model.AttentionNeedsInput, map[string]bool{"dev": true}, true},
		{"needs input, nobody present", model.AttentionNeedsInput, nil, false},
		{"done never alerts, even present", model.AttentionDone, map[string]bool{"dev": true}, false},
		{"idle is ambient", model.AttentionIdle, map[string]bool{"dev": true}, false},
		{"running is ambient", model.AttentionRunning, map[string]bool{"dev": true}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			p := presenceWithFocus(c.focused)
			s := &Server{presence: p, focus: p}
			frame := s.firehoseFrame(manager.Event{
				Kind: "attention", WorkspaceID: "ws1", PaneID: "p1", Attention: c.att,
			})
			if frame.Alert != c.want {
				t.Errorf("alert = %v, want %v", frame.Alert, c.want)
			}
			if frame.State != c.att || frame.Workspace != "ws1" {
				t.Errorf("frame lost its payload: %+v", frame)
			}
		})
	}
}

// Non-attention frames carry no alert, and must keep passing through untouched.
func TestFirehoseFrame_OtherKindsAreUnchanged(t *testing.T) {
	s := &Server{presence: presenceWithFocus(map[string]bool{"dev": true})}
	frame := s.firehoseFrame(manager.Event{Kind: "workspace-status", WorkspaceID: "ws1"})
	if frame.T != "workspace-status" || frame.Alert {
		t.Errorf("frame = %+v, want workspace-status with no alert", frame)
	}
}

// presenceWithFocus builds a hub whose ActiveOwners is exactly the given logins.
func presenceWithFocus(logins map[string]bool) *presenceHub {
	h := newPresenceHub(manager.New(context.Background(), nil, nil))
	for login := range logins {
		id := h.Join("ws1", ClientInfo{User: login}, login, "")
		h.Focus("ws1", id, "some-pane")
	}
	return h
}
