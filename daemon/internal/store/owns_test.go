package store

import "testing"

// A repo's line is the human's when set, else the summary; writing one
// never clears the other, and "" hands the repo back to the summary.
func TestRepoOwns_ManualWinsAndTheSummaryKeepsItsHash(t *testing.T) {
	s := openTestStore(t)
	if o, err := s.RepoOwns("/r/backend"); err != nil || o != (RepoOwns{}) {
		t.Fatalf("never summarized = %+v, %v", o, err)
	}
	if err := s.SetRepoOwnsAuto("/r/backend", "API and database", "h1", 1); err != nil {
		t.Fatal(err)
	}
	if err := s.SetRepoOwnsManual("/r/backend", "API, database, billing", 2); err != nil {
		t.Fatal(err)
	}
	o, _ := s.RepoOwns("/r/backend")
	if o.Line() != "API, database, billing" || o.Auto != "API and database" || o.AutoHash != "h1" {
		t.Fatalf("after a manual line: %+v", o)
	}
	if err := s.SetRepoOwnsAuto("/r/backend", "API, database, jobs", "h2", 3); err != nil {
		t.Fatal(err)
	}
	if o, _ = s.RepoOwns("/r/backend"); o.Manual != "API, database, billing" || o.AutoHash != "h2" {
		t.Fatalf("a new summary must keep the manual line: %+v", o)
	}
	s.SetRepoOwnsManual("/r/backend", "", 4)
	if o, _ = s.RepoOwns("/r/backend"); o.Line() != "API, database, jobs" {
		t.Fatalf("clearing the manual line shows the summary: %+v", o)
	}
}
