package jobs

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// jobDir writes a spec for kind and returns its path.
func jobDir(t *testing.T, sp Spec) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), sp.ID, "spec.json")
	if err := WriteSpec(path, sp); err != nil {
		t.Fatal(err)
	}
	return path
}

func resultOf(t *testing.T, spec string) Result {
	t.Helper()
	res, err := ReadResult(filepath.Join(filepath.Dir(spec), "result.json"))
	if err != nil {
		t.Fatal(err)
	}
	return res
}

func TestAJobRunsItsKindAndWritesItsResultOnce(t *testing.T) {
	spec := jobDir(t, Spec{ID: "j1", Kind: "count", Input: []byte(`{"to":3}`)})
	runs := 0
	kinds := map[string]Runner{"count": func(r *Run) (any, error) {
		runs++
		for _, step := range []string{"one", "two"} {
			if r.Journal.Done(step, nil) {
				continue
			}
			r.Step(step, "counting "+step)
			r.Finished(step, nil)
		}
		return map[string]int{"n": 2}, nil
	}}
	if err := Exec(spec, kinds); err != nil {
		t.Fatal(err)
	}
	res := resultOf(t, spec)
	if res.ID != "j1" || res.Kind != "count" || res.Error != "" || !strings.Contains(string(res.Output), `"n": 2`) || res.EndedAt == 0 {
		t.Fatalf("the result carries what the kind returned: %+v %s", res, res.Output)
	}
	if err := Exec(spec, kinds); err != nil || runs != 1 {
		t.Fatalf("a job that has its result is not run again: %d runs, %v", runs, err)
	}
}

func TestARerunSkipsWhatTheJournalSaysIsDone(t *testing.T) {
	spec := jobDir(t, Spec{ID: "j1", Kind: "steps"})
	var did []string
	crash := true
	kinds := map[string]Runner{"steps": func(r *Run) (any, error) {
		for _, step := range []string{"rebase", "checks", "land"} {
			if r.Journal.Done(step, nil) {
				continue
			}
			if step == "land" && crash {
				panic("killed") // the process dies before the last step
			}
			did = append(did, step)
			r.Finished(step, nil)
		}
		return "landed", nil
	}}
	func() { // the first run, as a process that dies in the middle
		defer func() { recover() }()
		journal, _ := OpenJournal(filepath.Join(filepath.Dir(spec), "journal.jsonl"))
		kinds["steps"](&Run{Journal: journal})
	}()
	if _, err := ReadResult(filepath.Join(filepath.Dir(spec), "result.json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("a run that died left no result: %v", err)
	}
	crash = false
	if err := Exec(spec, kinds); err != nil {
		t.Fatal(err)
	}
	if strings.Join(did, ",") != "rebase,checks,land" {
		t.Fatalf("each step ran once across the two runs: %v", did)
	}
}

func TestAJobThisBinaryCannotRunSaysSoInItsResult(t *testing.T) {
	spec := jobDir(t, Spec{ID: "j1", Kind: "unknown"})
	if err := Exec(spec, map[string]Runner{}); err != nil {
		t.Fatal(err)
	}
	if res := resultOf(t, spec); !strings.Contains(res.Error, `runs no job of kind "unknown"`) {
		t.Fatalf("an unknown kind is refused in the result: %+v", res)
	}
	other := filepath.Join(t.TempDir(), "j2", "spec.json")
	os.MkdirAll(filepath.Dir(other), 0o700)
	os.WriteFile(other, []byte(`{"version":99,"id":"j2","kind":"count","input":{}}`), 0o600)
	ran := false
	if err := Exec(other, map[string]Runner{"count": func(*Run) (any, error) { ran = true; return nil, nil }}); err != nil {
		t.Fatal(err)
	}
	if res := resultOf(t, other); ran || !strings.Contains(res.Error, "format 99") || res.ID != "j2" {
		t.Fatalf("a spec in another format is not run, and the result says why: ran=%v %+v", ran, res)
	}
}

func TestAJobPastItsTimeLimitEndsWithAResultThatSaysSo(t *testing.T) {
	spec := jobDir(t, Spec{ID: "j1", Kind: "slow", MaxSeconds: 1})
	kinds := map[string]Runner{"slow": func(r *Run) (any, error) {
		<-r.Ctx.Done() // the work notices and stops
		return nil, r.Ctx.Err()
	}}
	start := time.Now()
	if err := Exec(spec, kinds); err != nil {
		t.Fatal(err)
	}
	if res := resultOf(t, spec); !strings.Contains(res.Error, "past its time limit of 1s") {
		t.Fatalf("the result says the limit stopped it: %+v", res)
	}
	if took := time.Since(start); took > 5*time.Second {
		t.Fatalf("work that stops when told is not waited for: %s", took)
	}
	// Work that ends well just as the limit comes still counts.
	late := jobDir(t, Spec{ID: "j2", Kind: "late", MaxSeconds: 1})
	if err := Exec(late, map[string]Runner{"late": func(r *Run) (any, error) { <-r.Ctx.Done(); return "done", nil }}); err != nil {
		t.Fatal(err)
	}
	if res := resultOf(t, late); res.Error != "" || !strings.Contains(string(res.Output), "done") {
		t.Fatalf("work that finished is not called late: %+v", res)
	}
}

func TestTheWorkRunsInTheEnvironmentTheSpecCarries(t *testing.T) {
	t.Setenv("ONLY_THE_JOBS", "x")
	t.Setenv("INVOCATION_ID", "the-jobs-own")
	t.Setenv("PATH", os.Getenv("PATH"))
	spec := jobDir(t, Spec{ID: "j1", Kind: "env", Env: []string{"PATH=/daemon/bin:/usr/bin", "FROM_DAEMON=1", "INVOCATION_ID=the-daemons"}})
	var got map[string]string
	if err := Exec(spec, map[string]Runner{"env": func(*Run) (any, error) {
		got = map[string]string{"PATH": os.Getenv("PATH"), "FROM_DAEMON": os.Getenv("FROM_DAEMON"), "ONLY_THE_JOBS": os.Getenv("ONLY_THE_JOBS"), "INVOCATION_ID": os.Getenv("INVOCATION_ID")}
		return nil, nil
	}}); err != nil {
		t.Fatal(err)
	}
	if got["PATH"] != "/daemon/bin:/usr/bin" || got["FROM_DAEMON"] != "1" || got["ONLY_THE_JOBS"] != "" {
		t.Fatalf("the daemon's environment replaces the service's: %v", got)
	}
	if got["INVOCATION_ID"] != "the-jobs-own" {
		t.Fatalf("what systemd says of this service itself stays its own: %v", got)
	}
}
