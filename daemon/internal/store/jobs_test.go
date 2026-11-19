package store

import (
	"errors"
	"path/filepath"
	"testing"
)

func TestASubjectHasOneOpenJobAndItsRowSaysHowItEnded(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "reg.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	first := Job{ID: "merge-a", Kind: "thread-merge", Subject: "thread:3", State: JobRunning, Ctx: `{"by":"lead"}`, StartedAt: 10}
	if err := st.AddJob(first); err != nil {
		t.Fatal(err)
	}
	if err := st.AddJob(Job{ID: "merge-b", Kind: "thread-merge", Subject: "thread:3", State: JobRunning, StartedAt: 11}); !errors.Is(err, ErrJobOpen) {
		t.Fatalf("a second job for the same subject is refused while the first is open: %v", err)
	}
	if err := st.AddJob(Job{ID: "merge-c", Kind: "thread-merge", Subject: "thread:4", State: JobRunning, StartedAt: 12}); err != nil {
		t.Fatalf("another subject's job is its own: %v", err)
	}
	open, err := st.OpenJobs()
	if err != nil || len(open) != 2 || open[0].ID != "merge-a" || open[0].Ctx != `{"by":"lead"}` {
		t.Fatalf("the open jobs, oldest first, with what their starter kept: %+v %v", open, err)
	}
	first.State, first.Outcome, first.Note, first.Attempts, first.EndedAt = JobDone, JobLost, "it died three times", 3, 20
	if err := st.SaveJob(first); err != nil {
		t.Fatal(err)
	}
	if got, err := st.OpenJob("thread:3"); err != nil || got != nil {
		t.Fatalf("a done job is not open: %+v %v", got, err)
	}
	if got, err := st.LastJob("thread:3"); err != nil || got == nil || got.Outcome != JobLost || got.Attempts != 3 || got.EndedAt != 20 {
		t.Fatalf("the last job says how it ended: %+v %v", got, err)
	}
	if err := st.AddJob(Job{ID: "merge-d", Kind: "thread-merge", Subject: "thread:3", State: JobRunning, StartedAt: 30}); err != nil {
		t.Fatalf("once the first is done the subject can have a new job: %v", err)
	}
	if got, _ := st.LastJob("thread:3"); got == nil || got.ID != "merge-d" {
		t.Fatalf("the newest job is the last: %+v", got)
	}
	if old, err := st.JobsEndedBefore(25); err != nil || len(old) != 1 || old[0].ID != "merge-a" {
		t.Fatalf("the done jobs that ended before a time: %+v %v", old, err)
	}
	if err := st.SaveJob(Job{ID: "nope"}); !errors.Is(err, ErrNoJob) {
		t.Fatalf("saving a job with no row is an error: %v", err)
	}
	if got, err := st.Job("nope"); err != nil || got != nil {
		t.Fatalf("an unknown job is nil: %+v %v", got, err)
	}
}
