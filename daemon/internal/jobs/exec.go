package jobs

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"

	"ccmux.dev/ccmuxd/internal/walls"
)

// Run is one run of a job, as its kind's Runner sees it.
type Run struct {
	Spec Spec
	// Journal is the job's own: the steps earlier runs finished are in it.
	Journal *Journal
	// Ctx ends at the job's time limit. Work that cannot be undone half way
	// asks it before it starts.
	Ctx  context.Context
	lock *Lock
}

// Unlock lets go of the repos' locks (LockRepos) once the work in them is
// over. A run that dies before it gets here needs no call: the kernel lets
// go for it.
func (r *Run) Unlock() {
	r.lock.Release()
	r.lock = nil
}

// LockRepos takes the lock of every repo given and holds it until Unlock,
// or until the job's process ends. While it waits for another job, the
// journal says so.
func (r *Run) LockRepos(repos []string) error {
	l, err := LockRepos(r.Ctx, r.Spec.Locks, repos, func(repo string) {
		name := filepath.Base(repo)
		r.Step("lock:"+repo, "waiting for another merge of "+name+" to end")
	})
	if err != nil {
		return err
	}
	r.lock = l
	return nil
}

// Step records that a step starts, for whoever watches the job. A journal
// that cannot be written is logged and the work goes on: every step is safe
// to repeat, so what is lost is time on a later run, never the work.
func (r *Run) Step(key, label string) {
	if err := r.Journal.Begin(key, label); err != nil {
		log.Printf("job %s: journal: %v", r.Spec.ID, err)
	}
}

// Finished records that a step is done, with what it wants remembered.
func (r *Run) Finished(key string, data any) {
	if err := r.Journal.Record(key, data); err != nil {
		log.Printf("job %s: journal: %v", r.Spec.ID, err)
	}
}

// Runner is what one kind of job does. Its output goes into the result
// file as it is; an error means the work could not be done at all.
type Runner func(r *Run) (output any, err error)

// Exec is the job-exec verb: it runs the job the spec file describes and
// writes its result beside the spec. It returns nil whenever a result was
// written, a bad one too: the result is the answer, and the daemon reads
// it. An error (and so a non-zero exit) means there is no result, and the
// daemon decides whether the job runs again.
func Exec(specPath string, kinds map[string]Runner) error {
	resultPath := filepath.Join(filepath.Dir(specPath), "result.json")
	if _, err := os.Stat(resultPath); err == nil {
		log.Printf("job: %s has its result already; nothing is run again", filepath.Dir(specPath))
		return nil
	}
	sp, err := ReadSpec(specPath)
	if err != nil {
		return refuse(resultPath, filepath.Base(filepath.Dir(specPath)), err)
	}
	run, ok := kinds[sp.Kind]
	if !ok {
		return refuse(resultPath, sp.ID, fmt.Errorf("this ccmuxd runs no job of kind %q", sp.Kind))
	}
	if err := checkWalls(sp); err != nil {
		return refuse(resultPath, sp.ID, err)
	}
	applyEnv(sp.Env)
	journal, err := OpenJournal(filepath.Join(filepath.Dir(specPath), "journal.jsonl"))
	if err != nil {
		return fmt.Errorf("job %s: its journal: %w", sp.ID, err)
	}
	log.Printf("job %s (%s, %s) starts", sp.ID, sp.Kind, sp.Subject)
	res := Result{ID: sp.ID, Kind: sp.Kind}
	out, err := runLimited(sp, journal, run)
	if err != nil {
		res.Error = err.Error()
	} else if res.Output, err = json.Marshal(out); err != nil {
		res.Error = "its output could not be written down: " + err.Error()
	}
	res.EndedAt = time.Now().UnixMilli()
	if err := WriteResult(resultPath, res); err != nil {
		return fmt.Errorf("job %s: its result was not written: %w", sp.ID, err)
	}
	log.Printf("job %s ends%s", sp.ID, errorNote(res.Error))
	return nil
}

// checkWalls proves a walled job's walls before any of its work runs
// (walls.Check). A job that is meant to be walled and is not ends with a
// result that says so: its kind's handler hands that to a person, and
// nothing of the job runs open.
func checkWalls(sp Spec) error {
	if sp.Walls == nil {
		return nil
	}
	if err := walls.Check(*sp.Walls); err != nil {
		return errors.New(walls.Refusal(err))
	}
	log.Printf("job %s: its walls are up", sp.ID)
	return nil
}

func errorNote(text string) string {
	if text == "" {
		return ""
	}
	return ": " + text
}

// refuse ends a job this binary cannot run (a spec in another format, a
// kind it does not know) with a result that says so, and the error itself:
// loudly in both places, and nothing of the spec is acted on.
func refuse(resultPath, id string, cause error) error {
	log.Printf("job %s: REFUSED: %v", id, cause)
	res := Result{ID: id, Error: "the job was not run: " + cause.Error(), EndedAt: time.Now().UnixMilli()}
	if err := WriteResult(resultPath, res); err != nil {
		return errors.Join(cause, err)
	}
	return nil
}

// limitGrace is how long a run that reached its time limit is given to
// stop by itself (its work is told through Ctx) before its result is
// written without it.
const limitGrace = 15 * time.Second

// runLimited runs the work under the spec's time limit. At the limit the
// work is told to stop and, when it has not within limitGrace, left behind:
// the process ends right after the result is written, and the service
// kills whatever the work had started. Work that still ended well in that
// time counts as done.
func runLimited(sp Spec, journal *Journal, run Runner) (any, error) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	limit := time.Duration(sp.MaxSeconds) * time.Second
	if limit > 0 {
		var stop context.CancelFunc
		ctx, stop = context.WithTimeout(ctx, limit)
		defer stop()
	}
	type ended struct {
		out any
		err error
	}
	done := make(chan ended, 1)
	go func() {
		out, err := run(&Run{Spec: sp, Journal: journal, Ctx: ctx})
		done <- ended{out, err}
	}()
	select {
	case e := <-done:
		return e.out, e.err
	case <-ctx.Done():
	}
	over := fmt.Errorf("it ran past its time limit of %s and was stopped", limit)
	select {
	case e := <-done:
		if e.err == nil {
			return e.out, nil
		}
		return nil, over
	case <-time.After(limitGrace):
		return nil, over
	}
}

// ownEnv is the variables systemd gives each service about itself. The job
// keeps its own of these and takes everything else from the spec.
var ownEnv = []string{"INVOCATION_ID", "JOURNAL_STREAM", "SYSTEMD_EXEC_PID", "NOTIFY_SOCKET", "MEMORY_PRESSURE_WATCH", "MEMORY_PRESSURE_WRITE", "MANAGERPID"}

// applyEnv makes this process's environment the one the spec carries (the
// daemon's own when it made the job), so what the job starts sees what it
// would have seen started by the daemon. An empty list changes nothing.
func applyEnv(env []string) {
	if len(env) == 0 {
		return
	}
	own := map[string]string{}
	for _, k := range ownEnv {
		if v, ok := os.LookupEnv(k); ok {
			own[k] = v
		}
	}
	os.Clearenv()
	for _, kv := range env {
		k, v, ok := strings.Cut(kv, "=")
		if !ok || isOwnEnv(k) {
			continue
		}
		os.Setenv(k, v)
	}
	for k, v := range own {
		os.Setenv(k, v)
	}
}

func isOwnEnv(key string) bool {
	for _, k := range ownEnv {
		if k == key {
			return true
		}
	}
	return false
}
