package agent

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestOpencodePortParse(t *testing.T) {
	if p := OpencodePort("CLAUDE_PEERS_NAME=x opencode --agent x --port 41234"); p != 41234 {
		t.Fatalf("got %d", p)
	}
	if p := OpencodePort("claude --name x"); p != 0 {
		t.Fatalf("non-opencode should be 0, got %d", p)
	}
}

func TestPushPromptAppendsThenSubmits(t *testing.T) {
	var calls []string
	var appended string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls = append(calls, r.Method+" "+r.URL.Path)
		if r.URL.Path == "/tui/append-prompt" {
			var b struct{ Text string }
			json.NewDecoder(r.Body).Decode(&b)
			appended = b.Text
		}
		w.WriteHeader(200)
	}))
	defer srv.Close()
	port, _ := strconv.Atoi(strings.TrimPrefix(srv.URL, "http://127.0.0.1:"))
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := PushPrompt(ctx, port, "hello agent"); err != nil {
		t.Fatal(err)
	}
	want := []string{"GET /session", "POST /tui/append-prompt", "POST /tui/submit-prompt"}
	if strings.Join(calls, ",") != strings.Join(want, ",") || appended != "hello agent" {
		t.Fatalf("calls %v appended %q", calls, appended)
	}
}

func TestPushPromptGivesUpWhenServerNeverAnswers(t *testing.T) {
	port, err := FreePort()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 700*time.Millisecond)
	defer cancel()
	if err := PushPrompt(ctx, port, "x"); err == nil {
		t.Fatal("expected a timeout error")
	}
}
