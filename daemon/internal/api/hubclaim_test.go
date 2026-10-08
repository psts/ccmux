package api

import (
	"encoding/json"
	"strings"
	"testing"

	"ccmux.dev/ccmuxd/internal/model"
)

// A hub re-encodes every relayed attention frame into firehoseMsg, so a claim
// survives the hop only while that struct models it. A frame with no claim
// gains none, and a frame that is not attention passes through untouched.
func TestRestampAlert_KeepsTheClaim(t *testing.T) {
	srv, _ := presenceServer(t)

	var got firehoseMsg
	out := srv.restampAlert([]byte(`{"t":"attention","workspace":"w","pane":"p","state":"needs_input","reason":"question","since":42}`), firehoseReader{})
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("relayed frame: %v", err)
	}
	if got.Reason != model.ReasonQuestion || got.Since != 42 || got.State != model.AttentionNeedsInput {
		t.Fatalf("relayed claim = %+v, want question since 42", got)
	}

	bare := string(srv.restampAlert([]byte(`{"t":"attention","workspace":"w","pane":"p","state":"idle"}`), firehoseReader{}))
	if strings.Contains(bare, "reason") || strings.Contains(bare, "since") {
		t.Fatalf("a claimless frame gained claim fields: %s", bare)
	}

	other := `{"t":"workspace-status","workspace":"w","reason":"kept as sent"}`
	if got := string(srv.restampAlert([]byte(other), firehoseReader{})); got != other {
		t.Fatalf("a non-attention frame was rewritten: %s", got)
	}
}

// The hub's notifier reads member frames through attentionEventFromFrame; the
// claim rides along there too.
func TestAttentionEventFromFrame_KeepsTheClaim(t *testing.T) {
	ev, ok := attentionEventFromFrame([]byte(`{"t":"attention","workspace":"w","pane":"p","state":"needs_input","reason":"permission","since":7}`))
	if !ok || ev.Reason != model.ReasonPermission || ev.Since != 7 {
		t.Fatalf("event = %+v ok=%v", ev, ok)
	}
	if _, ok := attentionEventFromFrame([]byte(`{"t":"workspace-status","workspace":"w"}`)); ok {
		t.Fatal("a non-attention frame became an attention event")
	}
}
