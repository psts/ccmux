package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// The thread tool hands the action to the daemon on the session's own pane,
// and says back what the daemon said: its words on success, its reason on a
// refusal.
func TestThreadTool(t *testing.T) {
	var got []string
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/panes/{id}/thread", func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		got = append(got, r.PathValue("id")+" "+string(body))
		if r.PathValue("id") != "lead-pane" {
			w.WriteHeader(http.StatusConflict)
			w.Write([]byte(`{"error":"the thread tool is the lead's: you are a helper in thread #3, so tell app-thread-3 what you need"}`))
			return
		}
		w.Write([]byte(`{"message":"Stood down backend-thread-3."}`))
	})
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)
	call := func(pane, args string) (string, bool) {
		a := &app{mcp: newMCPServerIO(strings.NewReader(""), &bytes.Buffer{}), paneID: pane}
		a.daemon = newDaemonClient(ts.URL, "")
		res := a.toolThread(json.RawMessage(args)).(map[string]any)
		isErr, _ := res["isError"].(bool)
		return res["content"].([]map[string]any)[0]["text"].(string), isErr
	}
	if text, isErr := call("lead-pane", `{"action":"stand_down","repo":"backend"}`); isErr || text != "Stood down backend-thread-3." {
		t.Fatalf("the lead reads what was done: %q %v", text, isErr)
	}
	if len(got) != 1 || got[0] != `lead-pane {"action":"stand_down","repo":"backend"}` {
		t.Fatalf("the pane is the identity, the arguments go through as they are: %v", got)
	}
	if text, isErr := call("helper-pane", `{"action":"merge"}`); !isErr || !strings.Contains(text, "the thread tool is the lead's") {
		t.Fatalf("a refusal is said with its reason: %q %v", text, isErr)
	}
	if text, isErr := call("", `{"action":"merge"}`); !isErr || !strings.Contains(text, "no pane") || len(got) != 2 {
		t.Fatalf("a session with no pane asks nothing of the daemon: %q %v %v", text, isErr, got)
	}
	if text, isErr := call("lead-pane", `not json`); !isErr || !strings.HasPrefix(text, "Bad arguments") {
		t.Fatalf("bad arguments are said: %q %v", text, isErr)
	}
}

// Every session carries the tool's description from its first turn, so it
// stays short.
func TestThreadToolTextIsShort(t *testing.T) {
	if n := len(threadToolText); n > 700 {
		t.Fatalf("the thread tool's description is %d characters; keep it under 700", n)
	}
	for _, action := range []string{"helpers", "stand_down", "merge", "stop_merge", "end"} {
		if !strings.Contains(threadToolText, action+" (") {
			t.Fatalf("the description explains %q", action)
		}
	}
}
