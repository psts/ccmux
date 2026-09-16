package main

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// check_messages renders chat, resolves a verdict through the permission
// notification, and acks any other kind (a question_answer belongs to the
// agent's chat server, which the daemon answers itself) without showing
// it as chat.
func TestCheckMessages_KindsByPoll(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/peers/poll", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"events":[
{"type":"message","seq":1,"from_id":"hq1","from_name":"hq","text":"please post it","sent_at":"2026-09-16T10:00:00Z"},
{"type":"permission_verdict","seq":2,"request_id":"abcde","behavior":"allow","from_id":"hq1"},
{"type":"question_answer","seq":3,"request_id":"bcdef","answer":"Dry","from_id":"hq1"},
{"type":"message","seq":4,"from_id":"hq1","from_name":"hq","text":"and then stop","sent_at":"2026-09-16T10:00:01Z"}]}`))
	})
	mux.HandleFunc("POST /v1/peers/tasks/list", func(w http.ResponseWriter, r *http.Request) { w.Write([]byte(`{"tasks":[]}`)) })
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)
	var out bytes.Buffer
	a := &app{mcp: newMCPServerIO(strings.NewReader(""), &out)}
	a.daemon = newDaemonClient(ts.URL, "")

	res := a.toolCheckMessages().(map[string]any)
	text := res["content"].([]map[string]any)[0]["text"].(string)
	if !strings.HasPrefix(text, "2 new message(s):") || !strings.Contains(text, "please post it") || !strings.Contains(text, "and then stop") {
		t.Errorf("chat not rendered as expected:\n%s", text)
	}
	for _, leaked := range []string{"question_answer", "Dry", "abcde", "bcdef"} {
		if strings.Contains(text, leaked) {
			t.Errorf("%q must not show as chat:\n%s", leaked, text)
		}
	}
	if !strings.Contains(out.String(), `"notifications/claude/channel/permission"`) || !strings.Contains(out.String(), `"request_id":"abcde"`) {
		t.Errorf("the verdict must resolve the dialog through the permission notification, got %s", out.String())
	}
	if !a.alreadyShown(4) {
		t.Error("every event, the answer included, must be acked so it is not polled again")
	}
}
