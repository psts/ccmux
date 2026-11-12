package main

import "testing"

// A part shows what it is built with once it has one; a part from before
// parts had a model shows as it always did.
func TestTaskListText(t *testing.T) {
	got := taskListText("Filed task #3.", []queuedTask{
		{ID: 2, Title: "Export", Status: "running", Parts: []queuedPart{
			{Repo: "app", Status: "running", BusName: "app-task-2"},
			{Repo: "backend", Status: "ready", BusName: "backend-task-2", Note: "route added", Model: "fable", Effort: "high"},
		}},
		{ID: 3, Title: "Settings", Status: "planning", AfterIDs: []int64{2}},
		{ID: 4, Title: "Billing", Status: "waiting", AfterIDs: []int64{3, 1}, Look: true},
	}, 0)
	want := "Filed task #3.\nTasks with a part in this repo:\n" +
		"- #2  Export  [running]\n    app: running (app-task-2)\n    backend: ready (backend-task-2)  built with fable, high effort  route added\n" +
		"- #3  Settings  [planning]  after #2\n" +
		"- #4  Billing  [waiting]  after #3, #1  (a person reads it before it ships)"
	if got != want {
		t.Fatalf("got\n%s\nwant\n%s", got, want)
	}
	if got := taskListText("", nil, 0); got != "No tasks with a part in this repo." {
		t.Fatalf("empty = %q", got)
	}
}

// The daemon lists only the latest finished tasks; the answer ends by
// saying how many it left out and how to see one.
func TestTaskListText_SaysWhatIsNotListed(t *testing.T) {
	got := taskListText("", []queuedTask{{ID: 30, Title: "Export", Status: "done"}}, 23)
	want := "Tasks with a part in this repo:\n- #30  Export  [done]\n" +
		"(23 older finished tasks are not listed; list with an id shows one whole.)"
	if got != want {
		t.Fatalf("got\n%s\nwant\n%s", got, want)
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
