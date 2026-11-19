package jobs

import (
	"os"
	"path/filepath"
	"testing"
)

func TestAJournalRemembersFinishedStepsAcrossRuns(t *testing.T) {
	path := filepath.Join(t.TempDir(), "job", "journal.jsonl")
	first, err := OpenJournal(path)
	if err != nil {
		t.Fatal(err)
	}
	if first.Done("checked:a@r1", nil) {
		t.Fatal("an empty journal has nothing done")
	}
	first.Begin("checked:a@r1", "a: checks running")
	if first.Done("checked:a@r1", nil) || !first.Cut("checked:a@r1") {
		t.Fatal("a step that began is not done, and reads as cut until it ends")
	}
	if err := first.Record("checked:a@r1", "lint failed once"); err != nil {
		t.Fatal(err)
	}
	first.Begin("checked:b@r2", "b: checks running") // the run is killed here

	second, err := OpenJournal(path)
	if err != nil {
		t.Fatal(err)
	}
	var flaked string
	if !second.Done("checked:a@r1", &flaked) || flaked != "lint failed once" {
		t.Fatalf("the next run finds the step done, with what it recorded: %q", flaked)
	}
	if second.Cut("checked:a@r1") || !second.Cut("checked:b@r2") || second.Done("checked:b@r2", nil) {
		t.Fatal("and the step the kill cut is cut, not done")
	}
	if second.Done("checked:a@r9", nil) {
		t.Fatal("the same work on another commit is another step")
	}
	var wrong struct{ N int }
	if second.Done("checked:a@r1", &wrong) {
		t.Fatal("a record the reader cannot use counts as not done: the step is repeated")
	}
	entries, _ := ReadJournal(path)
	if Doing(entries) != "b: checks running" {
		t.Fatalf("what the job is in the middle of is its last started step: %q", Doing(entries))
	}
	second.Record("checked:b@r2", nil)
	entries, _ = ReadJournal(path)
	if Doing(entries) != "" {
		t.Fatalf("between steps a job is in the middle of nothing: %q", Doing(entries))
	}
}

func TestALineACrashCutShortIsNotAStepAndDoesNotSpoilTheNext(t *testing.T) {
	path := filepath.Join(t.TempDir(), "journal.jsonl")
	j, _ := OpenJournal(path)
	j.Record("one", 1)
	f, _ := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0o600)
	f.WriteString(`{"key":"two","state":"done","da`) // power went here
	f.Close()

	again, err := OpenJournal(path)
	if err != nil {
		t.Fatal(err)
	}
	if !again.Done("one", nil) || again.Done("two", nil) {
		t.Fatal("the whole lines count, the cut one does not")
	}
	if err := again.Record("three", 3); err != nil {
		t.Fatal(err)
	}
	third, _ := OpenJournal(path)
	var n int
	if !third.Done("three", &n) || n != 3 || !third.Done("one", nil) || third.Done("two", nil) {
		t.Fatal("an entry written after the cut line is on a line of its own")
	}
}
