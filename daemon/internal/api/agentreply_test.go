package api

import (
	"net"
	"strconv"
	"strings"
	"testing"
	"time"

	"ccmux.dev/ccmuxd/internal/agent/agenttest"
)

// A card answered over REST (the attention board's tiles) reaches the
// agent's server exactly as the chat socket's answer frames do; anything
// that is not a card answer is refused, and so is a pane with no chat.
func TestAgentReply_AnswersCardsWithoutTheSocket(t *testing.T) {
	oc := agenttest.NewFakeOpencode()
	defer oc.Server.Close()
	f := newWindowAgentFixture(t, "sleep 4;: --port "+strconv.Itoa(oc.Port()))
	code, pane := f.start(t, "x-poster", "")
	if code != 201 {
		t.Fatalf("start = %d", code)
	}
	oc.Sessions = `[{"id":"ses_1","title":"now","directory":"` + pane.CWD + `","time":{"updated":9}}]`
	if rec := do(t, f.srv, "GET", "/v1/panes/"+pane.ID+"/agent", ""); !strings.Contains(rec.Body.String(), `"state":"running"`) {
		t.Fatalf("the agent must be live first: %d %s", rec.Code, rec.Body)
	}
	reply := "/v1/panes/" + pane.ID + "/agent/reply"
	for _, body := range []string{
		`{"t":"permission","id":"per_1","reply":"once"}`,
		`{"t":"question","id":"que_1","answers":[["Coffee"]]}`,
		`{"t":"question-reject","id":"que_2"}`,
	} {
		if rec := do(t, f.srv, "POST", reply, body); rec.Code != 200 {
			t.Fatalf("%s = %d %s", body, rec.Code, rec.Body)
		}
	}
	_, _, replies := oc.Recorded()
	a := oc.Answers()
	if replies["per_1"] != "once" || len(a["que_1"]) != 1 || a["que_1"][0][0] != "Coffee" {
		t.Fatalf("opencode saw replies %v answers %v", replies, a)
	}
	if got, ok := a["que_2"]; !ok || got != nil {
		t.Fatalf("the reject must reach opencode: %v (seen %v)", got, ok)
	}
	if prompts, _, _ := oc.Recorded(); len(prompts) != 0 {
		t.Fatalf("a reply route must never prompt: %v", prompts)
	}
	for _, body := range []string{`{"t":"prompt","text":"hi"}`, `{"t":"abort"}`, `{"t":"question","answers":[["x"]]}`, `not json`} {
		if rec := do(t, f.srv, "POST", reply, body); rec.Code != 400 {
			t.Fatalf("%s = %d, want 400", body, rec.Code)
		}
	}
	if rec := do(t, f.srv, "POST", "/v1/panes/"+f.ws.Panes[0].ID+"/agent/reply", `{"t":"question","id":"q"}`); rec.Code != 404 {
		t.Fatalf("a plain pane has no cards: %d", rec.Code)
	}
}

// A running agent whose server does not answer (busy, stuck) still holds
// its cards: 503 with the reason, never the asleep 409, which would read as
// nothing left to answer.
func TestAgentReply_ASilentServerIsNotAsleep(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := l.Addr().(*net.TCPAddr).Port
	l.Close() // nothing listens there now
	f := newWindowAgentFixture(t, "sleep 4;: --port "+strconv.Itoa(port))
	code, pane := f.start(t, "x-poster", "")
	if code != 201 {
		t.Fatalf("start = %d", code)
	}
	waitForegroundNotShell(t, f, pane.ID)
	rec := do(t, f.srv, "POST", "/v1/panes/"+pane.ID+"/agent/reply", `{"t":"question","id":"que_1","answers":[["a"]]}`)
	if rec.Code != 503 || !strings.Contains(rec.Body.String(), "did not answer") {
		t.Fatalf("silent server reply = %d %s, want 503", rec.Code, rec.Body)
	}
}

// An agent with no chat server (asleep, or a harness without one) has no
// cards: 409, not a 502 that reads like the server broke.
func TestAgentReply_AsleepIsAConflict(t *testing.T) {
	f := newWindowAgentFixture(t, "sleep 4;:")
	code, pane := f.start(t, "x-poster", "")
	if code != 201 {
		t.Fatalf("start = %d", code)
	}
	if rec := do(t, f.srv, "POST", "/v1/panes/"+pane.ID+"/agent/reply", `{"t":"question","id":"que_1","answers":[["a"]]}`); rec.Code != 409 {
		t.Fatalf("asleep reply = %d %s, want 409", rec.Code, rec.Body)
	}
}

// waitForegroundNotShell waits for the pane's harness to take the
// foreground, so the reply route sees a running agent.
func waitForegroundNotShell(t *testing.T, f *windowAgentFixture, pane string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for f.srv.mgr.PaneAtShell(pane) {
		if time.Now().After(deadline) {
			t.Fatal("the harness never took the foreground")
		}
		time.Sleep(50 * time.Millisecond)
	}
}
