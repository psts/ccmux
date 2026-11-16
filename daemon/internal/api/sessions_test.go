package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"ccmux.dev/ccmuxd/internal/runner"
	"ccmux.dev/ccmuxd/internal/store"
)

type fakeRunners struct {
	created  []string
	sent     []string
	wanted   []string
	models   []string
	restarts int
}

func (f *fakeRunners) Create(cwd, name, model string) (store.RunnerSession, error) {
	f.created = append(f.created, cwd)
	return store.RunnerSession{ID: "s1", Cwd: cwd, Wanted: store.SessionRunning}, nil
}

func (f *fakeRunners) Send(id, text, sender string) (string, error) {
	if id == "ended" {
		return "", runner.ErrSessionDone
	}
	if id != "s1" {
		return "", store.ErrNoSession
	}
	f.sent = append(f.sent, text)
	return "p1", nil
}

func (f *fakeRunners) SetWanted(id, wanted string) error {
	if id == "ended" {
		return runner.ErrSessionDone
	}
	f.wanted = append(f.wanted, wanted)
	return nil
}

func (f *fakeRunners) Restart(id string) error {
	if id == "ended" {
		return runner.ErrSessionDone
	}
	f.restarts++
	return nil
}

func (f *fakeRunners) SetModel(id, model string) error {
	if id == "ended" {
		return runner.ErrSessionDone
	}
	f.models = append(f.models, model)
	return nil
}

type fakeRunnerRows struct{}

func (fakeRunnerRows) RunnerSessions() ([]store.RunnerSession, error) { return nil, nil }
func (fakeRunnerRows) RunnerSession(id string) (*store.RunnerSession, error) {
	if id != "s1" {
		return nil, nil
	}
	return &store.RunnerSession{ID: "s1"}, nil
}
func (fakeRunnerRows) UndeliveredPrompts(string) ([]store.InboxPrompt, error) {
	return []store.InboxPrompt{{ID: "p1"}}, nil
}
func (fakeRunnerRows) OpenSessionCards(string) ([]store.SessionCard, error) { return nil, nil }

func callSessions(t *testing.T, s *Server, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, httptest.NewRequest(method, path, strings.NewReader(body)))
	return w
}

func TestSessionRoutesAreOffWithoutRunners(t *testing.T) {
	s := &Server{}
	if w := callSessions(t, s, "GET", "/v1/sessions", ""); w.Code != http.StatusServiceUnavailable {
		t.Fatalf("off without a supervisor: %d", w.Code)
	}
}

func TestSessionRoutes(t *testing.T) {
	f := &fakeRunners{}
	s := &Server{}
	s.SetRunners(f, fakeRunnerRows{})
	dir := t.TempDir()
	if w := callSessions(t, s, "POST", "/v1/sessions", `{"cwd":"`+dir+`"}`); w.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", w.Code, w.Body)
	}
	for _, bad := range []string{`{"cwd":"rel/dir"}`, `{"cwd":"/no/such/folder/here"}`} {
		if w := callSessions(t, s, "POST", "/v1/sessions", bad); w.Code != http.StatusBadRequest {
			t.Errorf("%s: want 400, got %d", bad, w.Code)
		}
	}
	if w := callSessions(t, s, "GET", "/v1/sessions", ""); w.Code != 200 || !strings.Contains(w.Body.String(), `"sessions":[]`) {
		t.Fatalf("list: %d %s", w.Code, w.Body)
	}
	if w := callSessions(t, s, "GET", "/v1/sessions/s1", ""); w.Code != 200 || !strings.Contains(w.Body.String(), `"waiting":1`) {
		t.Fatalf("one: %d %s", w.Code, w.Body)
	}
	if w := callSessions(t, s, "GET", "/v1/sessions/nope", ""); w.Code != http.StatusNotFound {
		t.Fatalf("unknown: %d", w.Code)
	}
	if w := callSessions(t, s, "POST", "/v1/sessions/s1/prompt", `{"text":"hi"}`); w.Code != http.StatusAccepted {
		t.Fatalf("prompt: %d %s", w.Code, w.Body)
	}
	if w := callSessions(t, s, "POST", "/v1/sessions/nope/prompt", `{"text":"hi"}`); w.Code != http.StatusNotFound {
		t.Fatalf("prompt to unknown: %d", w.Code)
	}
	if w := callSessions(t, s, "POST", "/v1/sessions/s1/wanted", `{"wanted":"sideways"}`); w.Code != http.StatusBadRequest {
		t.Fatalf("bad wanted: %d", w.Code)
	}
	for _, path := range []string{"/v1/sessions/ended/prompt", "/v1/sessions/ended/wanted"} {
		body := `{"text":"hi","wanted":"running"}`
		if w := callSessions(t, s, "POST", path, body); w.Code != http.StatusConflict {
			t.Errorf("%s on an ended session: want 409, got %d", path, w.Code)
		}
	}
	callSessions(t, s, "POST", "/v1/sessions/s1/wanted", `{"wanted":"asleep"}`)
	w := callSessions(t, s, "POST", "/v1/sessions/s1/model", `{"model":"haiku"}`)
	if w.Code != 200 || !strings.Contains(w.Body.String(), "on the next restart") || !strings.Contains(w.Body.String(), `"restart":"/v1/sessions/s1/restart"`) {
		t.Fatalf("a model says it applies on the next restart and names the restart: %d %s", w.Code, w.Body)
	}
	if w := callSessions(t, s, "POST", "/v1/sessions/s1/restart", ``); w.Code != http.StatusAccepted || f.restarts != 1 {
		t.Fatalf("restart now: %d %d", w.Code, f.restarts)
	}
	if w := callSessions(t, s, "POST", "/v1/sessions/ended/restart", ``); w.Code != http.StatusConflict {
		t.Fatalf("an ended session does not restart: %d", w.Code)
	}
	if w := callSessions(t, s, "POST", "/v1/sessions/ended/model", `{"model":"haiku"}`); w.Code != http.StatusConflict {
		t.Fatalf("an ended session takes no model: %d", w.Code)
	}
	if len(f.created) != 1 || len(f.sent) != 1 || len(f.wanted) != 1 || len(f.models) != 1 {
		t.Fatalf("each call reached the supervisor once: %+v", f)
	}
}
