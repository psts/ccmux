package taskq

import (
	"strings"
	"testing"

	"ccmux.dev/ccmuxd/internal/store"
)

// A report's lines are trimmed and blank ones dropped; more than three, a
// line over 140 characters or a line with a break in it is refused with
// what to do about it. Not saying is not the same as saying none.
func TestForYouLines(t *testing.T) {
	got, err := ForYouLines([]string{"  Prices change: check the table. ", "", "   "})
	if err != nil || len(got) != 1 || got[0] != "Prices change: check the table." {
		t.Fatalf("trimmed = %q, %v", got, err)
	}
	if got, err := ForYouLines(nil); got != nil || err != nil {
		t.Fatalf("nil = %q, %v; want nil (keep what it had)", got, err)
	}
	if got, err := ForYouLines([]string{}); got == nil || len(got) != 0 || err != nil {
		t.Fatalf("empty = %#v, %v; want an empty list (clear)", got, err)
	}
	if _, err := ForYouLines([]string{"a", "b", "c", "d"}); err == nil || !strings.Contains(err.Error(), "at most 3") {
		t.Fatalf("four lines: %v", err)
	}
	if _, err := ForYouLines([]string{strings.Repeat("é", 140)}); err != nil {
		t.Fatalf("140 characters (more bytes) refused: %v", err)
	}
	if _, err := ForYouLines([]string{strings.Repeat("x", 141)}); err == nil || !strings.Contains(err.Error(), "shorten") {
		t.Fatalf("141 characters: %v", err)
	}
	if _, err := ForYouLines([]string{"one\ntwo"}); err == nil {
		t.Fatal("a line with a break in it was taken")
	}
}

// A report's lines go on the part; a later report that does not say keeps
// them, an empty list clears them, and a part started again loses them.
func TestReport_ForYouLines(t *testing.T) {
	run := store.Part{ID: 1, Status: Running}
	got, err := Report(run, "done", "built", []string{"Drops old rows from sessions."}, 5)
	if err != nil || len(got.ForYou) != 1 {
		t.Fatalf("with a line = %q, %v", got.ForYou, err)
	}
	got.Status = Running
	if again, _ := Report(got, "done", "fixed", nil, 6); len(again.ForYou) != 1 {
		t.Fatalf("a report that did not say dropped the lines: %q", again.ForYou)
	}
	if cleared, _ := Report(got, "done", "fixed", []string{}, 6); len(cleared.ForYou) != 0 {
		t.Fatalf("an empty list kept the lines: %q", cleared.ForYou)
	}
	if _, err := Report(run, "done", "built", []string{"a", "b", "c", "d"}, 5); err == nil {
		t.Fatal("four lines were taken")
	}
	failed := store.Part{ID: 2, Status: Failed, ForYou: []string{"x"}}
	if again, _ := Retry(store.Task{Status: Running}, failed); again.ForYou != nil {
		t.Fatalf("a retried part kept its lines: %q", again.ForYou)
	}
	if again := Requeued([]store.Part{failed}); again[0].ForYou != nil {
		t.Fatalf("a requeued part kept its lines: %q", again[0].ForYou)
	}
}

// A task shows the mark only when a part has lines and a person has not
// read them. A task of two parts names the repo on each line.
func TestMarked(t *testing.T) {
	parts := []store.Part{
		{TaskID: 1, RepoPath: "/r/app"},
		{TaskID: 2, RepoPath: "/r/app", ForYou: []string{"Email goes to every customer."}},
		{TaskID: 2, RepoPath: "/r/api", ForYou: []string{"New public endpoint."}},
	}
	if Marked(store.Task{ID: 1, Look: true}, parts) {
		t.Fatal("a task with no lines is marked (the planner's look is only a hint)")
	}
	if !Marked(store.Task{ID: 2}, parts) {
		t.Fatal("a task with lines is not marked")
	}
	if Marked(store.Task{ID: 2, ForYouRead: true}, parts) {
		t.Fatal("a read task is still marked")
	}
	want := "app: Email goes to every customer.|api: New public endpoint."
	if got := strings.Join(ForYouOf(2, parts), "|"); got != want {
		t.Fatalf("lines = %q, want %q", got, want)
	}
	if got := ForYouOf(2, parts[2:]); len(got) != 1 || got[0] != "New public endpoint." {
		t.Fatalf("one part's lines = %q", got)
	}
}

// Every builder's brief has the For-you rule; the planner's look adds the
// hint and nothing else.
func TestBrief_ForYouRuleAndHint(t *testing.T) {
	part := store.Part{ID: 3, TaskID: 7, RepoPath: "/p/app", Plan: "the plan"}
	plain := store.Task{ID: 7, Title: "Export"}
	looked := store.Task{ID: 7, Title: "Export", Look: true}
	b := briefText(plain, part, layoutOf("/root", plain, part), "dev", nil, "")
	for _, want := range []string{`carry "read"`, "at most 3 short lines", "at most 140", "existing data changed or deleted",
		"money", "anything sent to people", "security", "Most tasks have none"} {
		if !strings.Contains(b, want) {
			t.Errorf("brief lacks %q", want)
		}
	}
	if strings.Contains(b, "Your planner thinks") {
		t.Error("a brief without look has the planner's hint")
	}
	hinted := briefText(looked, part, layoutOf("/root", looked, part), "dev", nil, "")
	if !strings.Contains(hinted, "Your planner thinks this task touches something on that list") {
		t.Error("a brief with look lacks the planner's hint")
	}
	if !strings.HasSuffix(hinted, "## Your plan\n\nthe plan\n") {
		t.Error("the plan is not last")
	}
}
