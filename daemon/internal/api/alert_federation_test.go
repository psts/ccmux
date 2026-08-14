package api

import (
	"encoding/json"
	"testing"

	"ccmux.dev/ccmuxd/internal/model"
)

// A member stamps the alert flag against its OWN lenses, which is the wrong
// question: the lens reading the hub's stream is attached to the HUB. A member
// with nobody looking at its own workspaces used to send alert=false for every
// pane it owned, and a Linux session's "needs input" arrived at the Mac as a
// silent sidebar flash.
func TestRestampAlert_HubOverridesAMembersVerdict(t *testing.T) {
	p := presenceWithFocus(map[string]bool{"dev": true})
	s := &Server{presence: p, focus: p}

	memberSaidNo := `{"t":"attention","workspace":"ws1","pane":"p1","state":"needs_input"}`
	out := s.restampAlert([]byte(memberSaidNo), "dev")

	var frame firehoseMsg
	if err := json.Unmarshal(out, &frame); err != nil {
		t.Fatalf("restamped frame is not decodable: %v (%s)", err, out)
	}
	if !frame.Alert {
		t.Error("the hub has a watcher, so a member's needs_input must alert")
	}
	if frame.Workspace != "ws1" || frame.Pane != "p1" || frame.State != model.AttentionNeedsInput {
		t.Errorf("re-stamping altered the payload: %s", out)
	}
}

// The override runs both ways. A member that claimed alert=true must not get one
// past the hub when the lens reading this stream belongs to somebody who is not
// at a screen.
func TestRestampAlert_HubAlsoWithdrawsAnAlert(t *testing.T) {
	s := &Server{presence: presenceWithFocus(nil), focus: presenceWithFocus(nil)}

	memberSaidYes := `{"t":"attention","workspace":"ws1","pane":"p1","state":"needs_input","alert":true}`
	out := s.restampAlert([]byte(memberSaidYes), "dev")

	var frame firehoseMsg
	if err := json.Unmarshal(out, &frame); err != nil {
		t.Fatal(err)
	}
	if frame.Alert {
		t.Error("this reader is not at a screen — the member's alert must be withdrawn")
	}
}

// It is a relay. Anything that is not an attention frame, or is not parseable at
// all, has to cross untouched — rewriting a frame you do not understand is how a
// relay corrupts a protocol it was only meant to carry.
func TestRestampAlert_PassesEverythingElseThrough(t *testing.T) {
	p := presenceWithFocus(map[string]bool{"dev": true})
	s := &Server{presence: p, focus: p}

	for _, raw := range []string{
		`{"t":"workspace-status","workspace":"ws1"}`,
		`{"t":"hello","attention":[{"workspace":"ws1","pane":"p1","state":"needs_input"}]}`,
		`not json at all`,
		``,
	} {
		if got := string(s.restampAlert([]byte(raw), "dev")); got != raw {
			t.Errorf("frame was rewritten:\n in: %s\nout: %s", raw, got)
		}
	}
}

// done never alerts, whoever is watching. The re-stamp must apply the same rule
// the hub's own frames get, not merely copy presence into the flag.
func TestRestampAlert_AppliesTheWholeRuleNotJustPresence(t *testing.T) {
	p := presenceWithFocus(map[string]bool{"dev": true})
	s := &Server{presence: p, focus: p}

	out := s.restampAlert([]byte(`{"t":"attention","workspace":"ws1","pane":"p1","state":"done","alert":true}`), "dev")
	var frame firehoseMsg
	if err := json.Unmarshal(out, &frame); err != nil {
		t.Fatal(err)
	}
	if frame.Alert {
		t.Error("done flashes the lens but never alerts, however many people are watching")
	}
}
