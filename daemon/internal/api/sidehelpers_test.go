package api

import (
	"net/http"
	"testing"
)

// Start over is the runner's: the route answers 202 when it started over
// and 409 when it was refused (Claude in a turn, an ended session). Standing
// a main session's helpers down follows from the runner (CleanStarted),
// never from the route, so a refused one keeps them.
func TestStartOverAsksTheRunner(t *testing.T) {
	s, runners, _ := sessionsServer(t, t.TempDir())
	f := &fakeThreads{}
	s.SetThreads(f)
	if w := callSessions(t, s, "POST", "/v1/sessions/s1/start-over", ""); w.Code != http.StatusAccepted {
		t.Fatalf("start over = %d %s", w.Code, w.Body)
	}
	for id, want := range map[string]int{"busy": http.StatusConflict, "ended": http.StatusConflict} {
		if w := callSessions(t, s, "POST", "/v1/sessions/"+id+"/start-over", ""); w.Code != want {
			t.Fatalf("%s = %d %s, want %d", id, w.Code, w.Body, want)
		}
	}
	if len(runners.startedOver) != 1 || runners.startedOver[0] != "s1" {
		t.Fatalf("started over = %v", runners.startedOver)
	}
	if got := f.movedOnNow(); len(got) != 0 {
		t.Fatalf("the route itself stands no helper down: %v", got)
	}
}
