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

// The todo tool hands the action to the daemon on the session's own pane,
// signed with the session's bus name, and says back what the daemon said.
func TestTodoTool(t *testing.T) {
	var got []string
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/panes/{id}/todo", func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		got = append(got, r.PathValue("id")+" "+string(body))
		var in struct{ Action string }
		_ = json.Unmarshal(body, &in)
		switch {
		case r.PathValue("id") == "agent-pane":
			w.WriteHeader(http.StatusBadRequest)
			w.Write([]byte(`{"error":"a role agent has no repo of its own: name the repo the item is about with repo"}`))
		case in.Action == "list":
			w.Write([]byte(`{"message":"","repo":"app","todos":[
				{"id":12,"text":"Fix the flaky test in store_test.go","urgency":"high","status":"open","addedBy":"app-task-7"},
				{"id":9,"text":"Update the readme","urgency":"medium","status":"tasked","taskId":41,"addedBy":"app"},
				{"id":4,"text":"Drop the dead flag","urgency":"low","status":"tasked"}]}`))
		default:
			w.Write([]byte(`{"message":"Added to-do #12 [high] to app's list.","repo":"app","todos":null}`))
		}
	})
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)
	call := func(pane, args string) (string, bool) {
		a := &app{mcp: newMCPServerIO(strings.NewReader(""), &bytes.Buffer{}), paneID: pane, name: "app-task-7"}
		a.daemon = newDaemonClient(ts.URL, "")
		res := a.toolTodo(json.RawMessage(args)).(map[string]any)
		isErr, _ := res["isError"].(bool)
		return res["content"].([]map[string]any)[0]["text"].(string), isErr
	}

	if text, isErr := call("pane-1", `{"action":"add","text":"Fix the flaky test in store_test.go","urgency":"high"}`); isErr || text != "Added to-do #12 [high] to app's list." {
		t.Fatalf("an add answers in one line: %q %v", text, isErr)
	}
	var sent map[string]any
	if len(got) != 1 || !strings.HasPrefix(got[0], "pane-1 ") || json.Unmarshal([]byte(strings.TrimPrefix(got[0], "pane-1 ")), &sent) != nil {
		t.Fatalf("the pane is the identity: %v", got)
	}
	if sent["by"] != "app-task-7" || sent["action"] != "add" || sent["urgency"] != "high" || sent["text"] != "Fix the flaky test in store_test.go" {
		t.Fatalf("the arguments go through as they are, signed with the bus name: %v", sent)
	}

	want := "app's to-do list, 3 unfinished:\n" +
		"- #12 [high] Fix the flaky test in store_test.go  added by app-task-7\n" +
		"- #9 [medium] Update the readme  (task #41 has it)  added by app\n" +
		"- #4 [low] Drop the dead flag  (being filed as a task)"
	if text, isErr := call("pane-1", `{"action":"list"}`); isErr || text != want {
		t.Fatalf("a list:\n got %q\nwant %q", text, want)
	}
	if text, isErr := call("agent-pane", `{"action":"add","text":"x"}`); !isErr || !strings.Contains(text, "name the repo") {
		t.Fatalf("a refusal is said with its reason: %q %v", text, isErr)
	}
	asked := len(got)
	if text, isErr := call("", `{"action":"list"}`); !isErr || !strings.Contains(text, "no pane") || len(got) != asked {
		t.Fatalf("a session with no pane asks nothing of the daemon: %q %v", text, isErr)
	}
	if text, isErr := call("pane-1", `not json`); !isErr || !strings.HasPrefix(text, "Bad arguments") || len(got) != asked {
		t.Fatalf("bad arguments are said, and not sent: %q %v", text, isErr)
	}
	// A caller cannot sign as someone else: the shim's own name wins.
	call("pane-1", `{"action":"done","id":12,"by":"the-human"}`)
	if !strings.Contains(got[len(got)-1], `"by":"app-task-7"`) {
		t.Fatalf("a by in the arguments replaced the session's own name: %s", got[len(got)-1])
	}
}

func TestTodoListText_AnEmptyListSaysSo(t *testing.T) {
	if got := todoListText("app", nil); got != "Nothing unfinished on app's to-do list." {
		t.Fatalf("empty list = %q", got)
	}
}

// Every session carries the tool's description from its first turn, and it
// is what tells a session whose instructions never reach it (an opencode
// agent's are dropped) when to add: it must say so, and stay short.
func TestTodoToolTextSaysWhenToAddAndIsShort(t *testing.T) {
	if n := len(todoToolText); n > 800 {
		t.Fatalf("the todo tool's description is %d characters; keep it under 800", n)
	}
	for _, want := range []string{"without being asked", "NOT handling now", "high if", "medium if", "low otherwise", "add (", "list (", "done ("} {
		if !strings.Contains(todoToolText, want) {
			t.Errorf("the description lost %q", want)
		}
	}
}

// The tasks tool's file action can name the to-do item it is for, or the
// several a planner merged into one task.
func TestTasksToolSchemaNamesTheTodosItFiles(t *testing.T) {
	for _, tool := range toolsList {
		if tool["name"] != "tasks" {
			continue
		}
		props := tool["inputSchema"].(map[string]any)["properties"].(map[string]any)
		todo, ok := props["todo"].(map[string]any)
		if !ok {
			t.Fatalf("tasks has no todo property: %v", props["todo"])
		}
		forms, _ := todo["anyOf"].([]map[string]any)
		if len(forms) != 2 || forms[0]["type"] != "integer" || forms[1]["type"] != "array" || forms[1]["items"].(map[string]any)["type"] != "integer" {
			t.Fatalf("todo takes %v, want a number or a list of numbers", todo["anyOf"])
		}
		if desc, _ := todo["description"].(string); !strings.Contains(desc, "a list of numbers when one task covers several") {
			t.Fatalf("the description does not say when to send a list: %q", desc)
		}
		return
	}
	t.Fatal("no tasks tool")
}

// An item in a planning line reads as taken: nobody starts on it by hand.
func TestTodoLine_SaysWhoHasAnItem(t *testing.T) {
	for status, want := range map[string]string{"planning": "(in the planner's line)", "tasked": "(being filed as a task)", "open": ""} {
		got := todoLine(listedTodo{ID: 3, Urgency: "low", Text: "Fix it", Status: status})
		if want == "" && strings.Contains(got, "(") || !strings.Contains(got, want) {
			t.Errorf("%s: %q, want it to say %q", status, got, want)
		}
	}
	if got := todoLine(listedTodo{ID: 3, Urgency: "low", Text: "Fix it", Status: "tasked", TaskID: 41}); !strings.Contains(got, "(task #41 has it)") {
		t.Errorf("an item with a task: %q", got)
	}
}
