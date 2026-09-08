package api

import (
	"net/http/httptest"
	"strings"
	"testing"

	"ccmux.dev/ccmuxd/internal/model"
)

func TestAgentSignal_ValidationAndUnknownPane(t *testing.T) {
	s := agentsServer(t)
	post := func(remote, body string) int {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest("POST", "/v1/panes/p-unknown/agent-signal", strings.NewReader(body))
		req.RemoteAddr = remote
		s.Handler().ServeHTTP(rec, req)
		return rec.Code
	}
	if code := post("10.0.0.9:1234", `{"state":"idle"}`); code != 403 && code != 401 {
		t.Errorf("non-loopback should be refused, got %d", code)
	}
	if code := post("127.0.0.1:1234", `{"state":"sleepy"}`); code != 400 {
		t.Errorf("bad state = %d", code)
	}
	if code := post("127.0.0.1:1234", `{"state":"idle"}`); code != 404 {
		t.Errorf("unknown pane = %d", code)
	}
}

func TestAgentSignal_OutcomeTable(t *testing.T) {
	for state, want := range map[string]struct {
		att  model.Attention
		busy bool
	}{
		"busy": {model.AttentionRunning, true}, "idle": {model.AttentionDone, false},
		"needs-input": {model.AttentionNeedsInput, true}, "replied": {model.AttentionRunning, true},
	} {
		att, busy, ok := agentSignalOutcome(state)
		if !ok || att != want.att || busy != want.busy {
			t.Errorf("%s = %s, %v, %v; want %s, %v", state, att, busy, ok, want.att, want.busy)
		}
	}
	if _, _, ok := agentSignalOutcome("sleepy"); ok {
		t.Error("unknown state accepted")
	}
}
