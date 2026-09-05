package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"ccmux.dev/ccmuxd/internal/agent"
)

func agentsServer(t *testing.T) *Server {
	s := settingsServer(t)
	s.SetAgents(agent.NewStore(filepath.Join(t.TempDir(), "agents")))
	return s
}

func do(t *testing.T, s *Server, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	s.Handler().ServeHTTP(rec, req)
	return rec
}

func TestAgents_CRUDAndValidation(t *testing.T) {
	s := agentsServer(t)
	if rec := do(t, s, "GET", "/v1/agents", ""); rec.Code != 200 || !strings.Contains(rec.Body.String(), `"agents":[]`) {
		t.Fatalf("empty list: %d %s", rec.Code, rec.Body)
	}
	rec := do(t, s, "PUT", "/v1/agents/x-poster", `{"description":"Posts X threads about releases.","instructions":"# Role\n"}`)
	if rec.Code != 200 {
		t.Fatalf("put = %d %s", rec.Code, rec.Body)
	}
	var d agent.Definition
	json.Unmarshal(rec.Body.Bytes(), &d)
	if d.Name != "x-poster" || d.Version != "1.0.0" || d.Harness != agent.DefaultHarness || d.Instructions != "# Role\n" {
		t.Fatalf("stored = %+v", d)
	}
	if rec := do(t, s, "PUT", "/v1/agents/x-poster", `{"description":""}`); rec.Code != 400 {
		t.Fatalf("empty description should 400, got %d", rec.Code)
	}
	if rec := do(t, s, "PUT", "/v1/agents/x-poster", `{"description":"d","harness":"no-such-harness"}`); rec.Code != 400 || !strings.Contains(rec.Body.String(), "harness") {
		t.Fatalf("unknown harness should 400 naming it, got %d %s", rec.Code, rec.Body)
	}
	if rec := do(t, s, "PUT", "/v1/agents/x-poster", `{"description":"d","harness":"claude"}`); rec.Code != 200 {
		t.Fatalf("builtin harness should be accepted, got %d %s", rec.Code, rec.Body)
	}
	if rec := do(t, s, "GET", "/v1/agents", ""); !strings.Contains(rec.Body.String(), `"name":"x-poster"`) {
		t.Fatalf("list after put: %s", rec.Body)
	}
	if rec := do(t, s, "DELETE", "/v1/agents/x-poster", ""); rec.Code != http.StatusNoContent {
		t.Fatalf("delete = %d", rec.Code)
	}
	if rec := do(t, s, "DELETE", "/v1/agents/x-poster", ""); rec.Code != 404 {
		t.Fatalf("second delete = %d", rec.Code)
	}
}

func TestAgents_UnavailableWithoutStore(t *testing.T) {
	s := settingsServer(t)
	for _, c := range []struct{ m, p string }{{"GET", "/v1/agents"}, {"PUT", "/v1/agents/a-b-c"}, {"DELETE", "/v1/agents/a-b-c"}} {
		if rec := do(t, s, c.m, c.p, `{"description":"d"}`); rec.Code != 503 {
			t.Errorf("%s %s = %d, want 503", c.m, c.p, rec.Code)
		}
	}
}
