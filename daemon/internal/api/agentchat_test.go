package api

import (
	"context"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"ccmux.dev/ccmuxd/internal/agent"
	"ccmux.dev/ccmuxd/internal/agent/agenttest"
)

// The chat socket on a RUNNING agent: hello carries the newest session in
// the instance folder with its transcript and waiting permissions, a prompt
// goes to opencode's prompt_async, events stream back as transcript
// frames, a permission reply reaches opencode, and the pane returning to
// its shell turns the view asleep with the offline history.
func TestAgentChat_LiveThenAsleep(t *testing.T) {
	oc := agenttest.NewFakeOpencode()
	defer oc.Server.Close()
	// The harness line carries opencode's --port the way a real launch does;
	// sleep is the foreground so the pane is not at its shell.
	f := newWindowAgentFixture(t, "sleep 4;: --port "+strconv.Itoa(oc.Port()))
	f.srv.offlineSessions = func(context.Context, string) ([]agent.OpencodeSession, error) {
		return []agent.OpencodeSession{{ID: "ses_off", Title: "from the store"}}, nil
	}
	f.srv.offlineTranscript = func(context.Context, string) ([]agent.Turn, error) {
		return []agent.Turn{{ID: "m", Role: "user", Parts: []agent.TurnPart{{Type: "text", Text: "earlier"}}}}, nil
	}
	code, pane := f.start(t, "x-poster", "")
	if code != 201 {
		t.Fatalf("start = %d", code)
	}
	// opencode's sessions belong to the instance folder.
	oc.Sessions = `[{"id":"ses_1","title":"now","directory":"` + pane.CWD + `","time":{"updated":9}},{"id":"ses_x","title":"elsewhere","directory":"/other","time":{"updated":99}}]`
	if rec := do(t, f.srv, "GET", "/v1/panes/"+pane.ID+"/agent", ""); rec.Code != 200 || !strings.Contains(rec.Body.String(), `"session":"ses_1"`) {
		t.Fatalf("history = %d %s", rec.Code, rec.Body)
	}
	if rec := do(t, f.srv, "GET", "/v1/panes/"+f.ws.Panes[0].ID+"/agent", ""); rec.Code != 404 {
		t.Fatalf("a plain pane has no chat: %d", rec.Code)
	}

	conn := dialChat(t, f.base, pane.ID)
	defer conn.Close()
	hello := readChat(t, conn, "hello")
	if hello.State != "running" || hello.Session != "ses_1" || hello.Title != "now" || len(hello.Turns) != 2 || len(hello.Permissions) != 1 || hello.Permissions[0].ID != "per_1" {
		t.Fatalf("live hello: %+v", hello)
	}
	conn.WriteJSON(chatFrame{T: "prompt", Text: "and then?"})
	conn.WriteJSON(chatFrame{T: "permission", ID: "per_1", Reply: "always"})
	oc.Events <- `{"type":"message.updated","properties":{"sessionID":"ses_1","info":{"id":"msg_a2","role":"assistant","time":{"created":5}}}}`
	oc.Events <- `{"type":"message.part.delta","properties":{"sessionID":"ses_x","messageID":"m","partID":"p","field":"text","delta":"NOT MINE"}}`
	oc.Events <- `{"type":"message.part.updated","properties":{"sessionID":"ses_1","part":{"id":"prt_9","messageID":"msg_a2","type":"tool","tool":"read","state":{"status":"running","input":{"path":"a"},"title":"read a"}}}}`
	oc.Events <- `{"type":"session.idle","properties":{"sessionID":"ses_1"}}`
	turn := readChat(t, conn, "turn")
	if turn.Turn.ID != "msg_a2" || turn.Turn.Role != "assistant" {
		t.Fatalf("turn: %+v", turn.Turn)
	}
	part := readChat(t, conn, "part")
	if part.MessageID != "msg_a2" || part.Part.Tool != "read" || part.Part.Status != "running" {
		t.Fatalf("part: %+v", part)
	}
	readChat(t, conn, "idle")
	// The reader handles client frames on its own goroutine: give it a moment.
	deadline := time.Now().Add(5 * time.Second)
	for {
		prompts, _, replies := oc.Recorded()
		if len(prompts) == 1 && prompts[0] == "ses_1:and then?" && replies["per_1"] == "always" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("opencode saw prompts %v replies %v", prompts, replies)
		}
		time.Sleep(50 * time.Millisecond)
	}
	// sleep ends: the pane is at its shell, the view goes asleep with the
	// offline history.
	asleep := readChat(t, conn, "hello")
	if asleep.State != "asleep" || asleep.Session != "ses_off" || len(asleep.Turns) != 1 || asleep.Turns[0].Parts[0].Text != "earlier" {
		t.Fatalf("asleep hello: %+v", asleep)
	}
	// A prompt while asleep wakes the agent with it: starting, then live again.
	conn.WriteJSON(chatFrame{T: "prompt", Text: "wake up"})
	if st := readChat(t, conn, "state"); st.State != "starting" {
		t.Fatalf("after a prompt while asleep: %+v", st)
	}
	if again := readChat(t, conn, "hello"); again.State != "running" || again.Session != "ses_1" {
		t.Fatalf("woken hello: %+v", again)
	}
}

func dialChat(t *testing.T, base, paneID string) *websocket.Conn {
	t.Helper()
	url := "ws" + strings.TrimPrefix(base, "http") + "/v1/panes/" + paneID + "/agent/ws"
	conn, resp, err := websocket.DefaultDialer.Dial(url, nil)
	if err != nil {
		code := 0
		if resp != nil {
			code = resp.StatusCode
		}
		t.Fatalf("dial %s: %v (%d)", url, err, code)
	}
	return conn
}

// readFrame reads until a frame of kind t arrives (others in between are
// skipped), within a bound.
func readChat(t *testing.T, conn *websocket.Conn, kind string) chatFrame {
	t.Helper()
	conn.SetReadDeadline(time.Now().Add(10 * time.Second))
	for {
		var f chatFrame
		if err := conn.ReadJSON(&f); err != nil {
			t.Fatalf("waiting for %q: %v", kind, err)
		}
		if f.T == kind {
			return f
		}
		if f.T == "error" {
			t.Logf("error frame while waiting for %q: %s", kind, f.Error)
		}
	}
}
