package main

import "testing"

func TestTaskListText(t *testing.T) {
	got := taskListText("Filed task #3.", []queuedTask{
		{ID: 2, Title: "Export", Status: "running", Parts: []queuedPart{
			{Repo: "app", Status: "running", BusName: "app-task-2"},
			{Repo: "backend", Status: "ready", BusName: "backend-task-2", Note: "route added"},
		}},
		{ID: 3, Title: "Settings", Status: "planning", AfterID: 2},
	})
	want := "Filed task #3.\nTasks with a part in this repo:\n" +
		"- #2  Export  [running]\n    app: running (app-task-2)\n    backend: ready (backend-task-2)  route added\n" +
		"- #3  Settings  [planning]  after #2"
	if got != want {
		t.Fatalf("got\n%s\nwant\n%s", got, want)
	}
	if got := taskListText("", nil); got != "No tasks with a part in this repo." {
		t.Fatalf("empty = %q", got)
	}
}

// Every advertised tool has a handler, and nothing is handled that is not
// advertised: the table and toolsList are two lists that must not drift.
func TestToolHandlersMatchToolsList(t *testing.T) {
	for _, tool := range toolsList {
		if _, ok := toolHandlers[tool["name"].(string)]; !ok {
			t.Errorf("tool %v has no handler", tool["name"])
		}
	}
	if len(toolHandlers) != len(toolsList) {
		t.Errorf("%d handlers for %d tools", len(toolHandlers), len(toolsList))
	}
}
