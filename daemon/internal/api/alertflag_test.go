package api

import (
	"context"
	"net/http"
	"net/http/httptest"
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
			}, named("dev"))
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

	if !s.firehoseFrame(ev, named("here")).Alert {
		t.Error("the lens whose owner is at a screen must be told to alert")
	}
	if s.firehoseFrame(ev, named("away")).Alert {
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
	}, named(""))
	if frame.Alert {
		t.Error("a lens with no identity must not be told to alert")
	}
}

// Non-attention frames carry no alert, and must keep passing through untouched.
func TestFirehoseFrame_OtherKindsAreUnchanged(t *testing.T) {
	s := &Server{presence: presenceWithFocus(map[string]bool{"dev": true})}
	frame := s.firehoseFrame(manager.Event{Kind: "workspace-status", WorkspaceID: "ws1"}, named("dev"))
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

// presenceWithPresent builds a hub whose lenses report presence explicitly and
// focus NOTHING — the Mac three Spaces away, or showing a local workspace.
func presenceWithPresent(logins map[string]bool) *presenceHub {
	h := newPresenceHub(manager.New(context.Background(), nil, nil))
	for login, present := range logins {
		id := h.Join("ws1", ClientInfo{User: login}, login, "")
		h.SetPresent("ws1", id, present)
	}
	return h
}

// named builds a reader that told the daemon who it is — the modern lens.
func named(login string) firehoseReader {
	return firehoseReader{login: login, identified: true}
}

// A lens that never names itself on this socket falls back to the OLD global
// rule: alert if anybody is at a screen.
//
// This is the whole compatibility story for a staged rollout. The daemon and the
// Mac app ship separately, so there is always a window where an older app talks
// to an upgraded daemon. It does not send ?user= on the firehose, so it resolves
// to "anon" and can never match the login its own attach socket reports. Without
// this fallback, upgrading the daemon silently switches every notification off
// for that app — the exact bug the change is meant to fix.
func TestFirehoseFrame_UnidentifiedLensKeepsTheOldGlobalRule(t *testing.T) {
	ev := manager.Event{Kind: "attention", WorkspaceID: "ws1", PaneID: "p1", Attention: model.AttentionNeedsInput}
	old := firehoseReader{login: "anon", identified: false}

	present := presenceWithPresent(map[string]bool{"someone": true})
	s := &Server{presence: present, focus: present}
	if !s.firehoseFrame(ev, old).Alert {
		t.Error("an older lens must still be alerted while somebody is at a screen")
	}

	empty := newPresenceHub(manager.New(context.Background(), nil, nil))
	s = &Server{presence: empty, focus: empty}
	if s.firehoseFrame(ev, old).Alert {
		t.Error("with nobody at a screen even an older lens must not alert")
	}
}

// readerOf is where "identified" is decided, and only VERIFICATION counts.
//
// A self-declared name is not enough, because the per-reader rule joins this
// login against a presence entry written by a socket that may have resolved
// identity a completely different way (loopback name vs tailnet email). Treating
// "?user=Patric Sandelin" as identified would make that join fail closed and kill
// hosted alerts on the ordinary Mac-plus-remote-host setup.
func TestReaderFor_OnlyVerifiedCallersGetThePerReaderRule(t *testing.T) {
	s := &Server{mgr: manager.New(context.Background(), nil, nil), identity: declinedWhois{}}

	bare := httptest.NewRequest(http.MethodGet, "/v1/events", nil)
	if readerOf(s.resolveIdentity(bare)).identified {
		t.Error("an anonymous socket must not be treated as identified")
	}

	named := httptest.NewRequest(http.MethodGet, "/v1/events?user=dev", nil)
	if r := readerOf(s.resolveIdentity(named)); r.identified {
		t.Errorf("a self-declared name is not verification: %+v", r)
	}
}

// The regression this guards: a Mac whose firehose is loopback (unverified, named
// by NSFullUserName) while its presence was written by a tailnet attach under a
// verified email. The two logins never match, so a strict join would report no
// alert for somebody sitting right at the screen.
func TestAlertsFor_LoopbackMacStillAlertsWhilePresenceIsUnderAnotherLogin(t *testing.T) {
	present := presenceWithPresent(map[string]bool{"patric@example.com": true})
	s := &Server{presence: present, focus: present}

	loopback := firehoseReader{login: "Patric Sandelin", identified: false}
	if !s.alertsFor(loopback, "w1", model.AttentionNeedsInput) {
		t.Error("an unverified lens must fall back to the global rule, not fail closed")
	}
}

// declinedWhois stands in for a loopback caller, where tailnet identity declines.
type declinedWhois struct{}

func (declinedWhois) Resolve(string) (string, string, bool) { return "", "", false }
