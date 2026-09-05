package api

import (
	"net/http/httptest"
	"strings"
	"testing"
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
