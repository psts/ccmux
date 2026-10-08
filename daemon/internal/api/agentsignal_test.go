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
	for state, want := range map[string]agentSignalOutcome{
		"busy":        {model.AttentionRunning, model.ReasonNone, true},
		"idle":        {model.AttentionDone, model.ReasonFinished, false},
		"permission":  {model.AttentionNeedsInput, model.ReasonPermission, true},
		"question":    {model.AttentionNeedsInput, model.ReasonQuestion, true},
		"needs-input": {model.AttentionNeedsInput, model.ReasonNone, true},
		"replied":     {model.AttentionRunning, model.ReasonNone, true},
	} {
		got, ok := agentSignals[state]
		if !ok || got != want {
			t.Errorf("%s = %+v, %v; want %+v", state, got, ok, want)
		}
	}
	if _, ok := agentSignals["sleepy"]; ok {
		t.Error("unknown state accepted")
	}
}

// The handler on a real agent pane: needs-input badges the pane AND keeps
// the busy clock running (a single activity write, never the Claude
// reading first); idle ends both.
func TestAgentSignal_NeedsInputBadgesAndStaysBusy(t *testing.T) {
	f := newWindowAgentFixture(t, "sleep 4;:")
	code, pane := f.start(t, "x-poster", "")
	if code != 201 {
		t.Fatalf("start = %d", code)
	}
	post := func(state string) int {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest("POST", "/v1/panes/"+pane.ID+"/agent-signal", strings.NewReader(`{"state":"`+state+`"}`))
		req.RemoteAddr = "127.0.0.1:1234"
		f.srv.Handler().ServeHTTP(rec, req)
		return rec.Code
	}
	attention := func() string {
		for _, w := range getJSONList(t, f.base+"/v1/workspaces") {
			for _, p := range w["panes"].([]any) {
				if pm := p.(map[string]any); pm["id"] == pane.ID {
					att, _ := pm["attention"].(string)
					return att
				}
			}
		}
		return "?"
	}
	if code := post("needs-input"); code != 204 {
		t.Fatalf("needs-input = %d", code)
	}
	if att := attention(); att != string(model.AttentionNeedsInput) {
		t.Fatalf("attention after needs-input = %q", att)
	}
	if !f.srv.mgr.AgentBusy(pane.ID) {
		t.Fatal("a pane waiting on a human must read busy, or the idle cap ends it under the dialog")
	}
	if code := post("idle"); code != 204 {
		t.Fatalf("idle = %d", code)
	}
	if att := attention(); att != string(model.AttentionDone) {
		t.Fatalf("attention after idle = %q", att)
	}
	if f.srv.mgr.AgentBusy(pane.ID) {
		t.Fatal("idle must clear busy")
	}
}
