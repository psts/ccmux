package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"ccmux.dev/ccmuxd/internal/agent"
	"ccmux.dev/ccmuxd/internal/store"
)

// pastRows has an agent's session "a1", now in conversation "c3", and a
// repo's lead "l1".
type pastRows struct{ fakeRunnerRows }

func (pastRows) RunnerSession(id string) (*store.RunnerSession, error) {
	switch id {
	case "a1":
		return &store.RunnerSession{ID: "a1", Kind: store.SessionAgent, Cwd: "/agents/seo", Conversation: "c3cccccc"}, nil
	case "l1":
		return &store.RunnerSession{ID: "l1", Kind: "lead", Cwd: "/r/app"}, nil
	}
	return nil, nil
}

func newPastServer(t *testing.T) (*httptest.Server, *[]string) {
	t.Helper()
	asked := &[]string{}
	srv := &Server{ka: defaultKeepalive()}
	srv.SetRunners(&fakeRunners{}, pastRows{})
	srv.SetRunnerDoors(RunnerDoors{
		Dial:    func(string) SessionRunner { return nil },
		History: func(context.Context, store.RunnerSession) (json.RawMessage, error) { return json.RawMessage(`[]`), nil },
		Conversations: func(_ context.Context, sess store.RunnerSession) ([]agent.OpencodeSession, error) {
			*asked = append(*asked, "list "+sess.Cwd)
			return []agent.OpencodeSession{{ID: "c3cccccc", Title: "today", Updated: 3}, {ID: "c2bbbbbb", Title: "yesterday", Updated: 2}}, nil
		},
		Past: func(_ context.Context, sess store.RunnerSession, conv string) (json.RawMessage, error) {
			*asked = append(*asked, "past "+sess.Cwd+" "+conv)
			if conv == "deadbeef" {
				return nil, errors.New("no such conversation")
			}
			return json.RawMessage(`[{"info":{"id":"u1","role":"user"},"parts":[]}]`), nil
		},
	})
	hs := httptest.NewServer(srv.Handler())
	t.Cleanup(hs.Close)
	return hs, asked
}

func getPast(t *testing.T, url string, out any) int {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if out != nil {
		json.NewDecoder(resp.Body).Decode(out)
	}
	return resp.StatusCode
}

// An agent's session lists the conversations of its folder, newest first,
// with the one it runs marked; one of them reads as a snapshot.
func TestPastConversations_ListAndRead(t *testing.T) {
	hs, asked := newPastServer(t)
	var list struct {
		Current       string             `json:"current"`
		Conversations []pastConversation `json:"conversations"`
	}
	if code := getPast(t, hs.URL+"/v1/sessions/a1/conversations", &list); code != 200 {
		t.Fatalf("list = %d", code)
	}
	if list.Current != "c3cccccc" || len(list.Conversations) != 2 || !list.Conversations[0].Current || list.Conversations[1].Current || list.Conversations[1].Title != "yesterday" {
		t.Fatalf("list = %+v", list)
	}
	var frame streamFrame
	if code := getPast(t, hs.URL+"/v1/sessions/a1/conversations/c2bbbbbb", &frame); code != 200 {
		t.Fatalf("read = %d", code)
	}
	if frame.T != "snapshot" || frame.State != "past" || !strings.Contains(string(frame.Data), `"messages":[{"info":{"id":"u1"`) {
		t.Fatalf("frame = %+v %s", frame, frame.Data)
	}
	if got := strings.Join(*asked, "|"); got != "list /agents/seo|past /agents/seo c2bbbbbb" {
		t.Fatalf("doors asked %q", got)
	}
}

// Only an agent's session, only a plain id, and a conversation that cannot
// be read says so.
func TestPastConversations_Refusals(t *testing.T) {
	hs, asked := newPastServer(t)
	for path, want := range map[string]int{
		"/v1/sessions/l1/conversations":          404, // a repo's lead
		"/v1/sessions/nope/conversations":        404,
		"/v1/sessions/a1/conversations/..%2Fx":   400,
		"/v1/sessions/a1/conversations/--dir=x":  400,
		"/v1/sessions/a1/conversations/deadbeef": 502,
	} {
		if code := getPast(t, hs.URL+path, nil); code != want {
			t.Errorf("%s = %d, want %d", path, code, want)
		}
	}
	if got := strings.Join(*asked, "|"); got != "past /agents/seo deadbeef" {
		t.Fatalf("doors asked %q: a refused request reached the sidecar", got)
	}
}
