package api

import (
	"net/http"
	"testing"

	"ccmux.dev/ccmuxd/internal/store"
)

// A message that starts a new conversation, /clear or a clean start, stands
// the session's side helpers down before it is sent; any other message does
// not.
func TestANewConversationStandsSideHelpersDown(t *testing.T) {
	s, _, st := sessionsServer(t, t.TempDir())
	if err := st.AddRunnerSession(store.RunnerSession{ID: "s1", Cwd: "/r/app", Wanted: store.SessionRunning, Kind: store.SessionLead}); err != nil {
		t.Fatal(err)
	}
	f := &fakeThreads{}
	s.SetThreads(f)
	for _, body := range []string{`{"text":"what does backend return for GET /trades?"}`, `{"text":"please /clear the cache in redis"}`, `{"text":"/clearly not"}`} {
		if w := callSessions(t, s, "POST", "/v1/sessions/s1/prompt", body); w.Code != http.StatusAccepted {
			t.Fatalf("%s = %d %s", body, w.Code, w.Body)
		}
	}
	if len(f.movedOnNow()) != 0 {
		t.Fatalf("an ordinary message moves nothing on: %v", f.movedOn)
	}
	for _, body := range []string{`{"text":"/clear"}`, `{"text":"  /clear  "}`, `{"text":"start over","clean":"c1"}`} {
		if w := callSessions(t, s, "POST", "/v1/sessions/s1/prompt", body); w.Code != http.StatusAccepted {
			t.Fatalf("%s = %d %s", body, w.Code, w.Body)
		}
	}
	want := []string{"s1 cleared its conversation (/clear)", "s1 cleared its conversation (/clear)", "s1 started a clean conversation"}
	if len(f.movedOn) != 3 || f.movedOn[0] != want[0] || f.movedOn[1] != want[1] || f.movedOn[2] != want[2] {
		t.Fatalf("moved on = %v, want %v", f.movedOn, want)
	}
}
