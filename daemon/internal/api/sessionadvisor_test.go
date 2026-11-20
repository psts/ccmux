package api

import (
	"net/http"
	"slices"
	"strings"
	"testing"
)

func TestAnAdvisorIsSavedForTheNextStart(t *testing.T) {
	s, f, _ := sessionsServer(t, t.TempDir())
	w := callSessions(t, s, "POST", "/v1/sessions/s1/advisor", `{"advisor":"opus"}`)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "on the next restart") || !strings.Contains(w.Body.String(), `"restart":"/v1/sessions/s1/restart"`) {
		t.Fatalf("an advisor says it applies on the next restart and names the restart: %d %s", w.Code, w.Body)
	}
	if w := callSessions(t, s, "POST", "/v1/sessions/s1/advisor", `{"advisor":""}`); w.Code != http.StatusOK {
		t.Fatalf("back to the person's settings: %d %s", w.Code, w.Body)
	}
	if w := callSessions(t, s, "POST", "/v1/sessions/s1/advisor", `{"advisor":"--model"}`); w.Code != http.StatusBadRequest {
		t.Fatalf("only a listed advisor is taken: %d", w.Code)
	}
	if w := callSessions(t, s, "POST", "/v1/sessions/ended/advisor", `{"advisor":"opus"}`); w.Code != http.StatusConflict {
		t.Fatalf("an ended session takes no advisor: %d", w.Code)
	}
	if !slices.Equal(f.advisors, []string{"opus", ""}) {
		t.Fatalf("what reached the supervisor: %q", f.advisors)
	}
}

func TestANewSessionStartsWithTheAdvisorPicked(t *testing.T) {
	repo := t.TempDir()
	s, f, _ := sessionsServer(t, repo)
	if w := callSessions(t, s, "POST", "/v1/sessions", `{"cwd":"`+repo+`","advisor":"--x"}`); w.Code != http.StatusBadRequest || len(f.created) != 0 {
		t.Fatalf("only a listed advisor is taken: %d %s", w.Code, w.Body)
	}
	if w := callSessions(t, s, "POST", "/v1/sessions", `{"cwd":"`+repo+`","advisor":"fable"}`); w.Code != http.StatusCreated || len(f.created) != 1 || f.created[0].Advisor != "fable" {
		t.Fatalf("it is made with it: %d %s %+v", w.Code, w.Body, f.created)
	}
}
