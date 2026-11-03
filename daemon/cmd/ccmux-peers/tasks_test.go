package main

import "testing"

func TestTaskListText(t *testing.T) {
	got := taskListText("Filed task #3.", []queuedTask{
		{ID: 2, Title: "route", Status: "waiting"},
		{ID: 3, Title: "button", Status: "waiting", AfterID: 2, Note: "blocked by #2"},
	})
	want := "Filed task #3.\nThis repo's tasks:\n- #2  route  [waiting]\n- #3  button  [waiting]  after #2  blocked by #2"
	if got != want {
		t.Fatalf("got\n%s\nwant\n%s", got, want)
	}
	if got := taskListText("", nil); got != "No tasks for this repo." {
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
