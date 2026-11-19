package taskq

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"strconv"
	"strings"
	"time"

	"ccmux.dev/ccmuxd/internal/jobs"
	"ccmux.dev/ccmuxd/internal/store"
)

// A feature's merge is a job (internal/jobs): the landing itself (RunLand:
// rebase, checks, fast-forward, under each repo's lock) runs in a service
// of its own, and so does the tidy-up after it (RunTidy, tidyjob.go). The
// runner only starts them and acts on how they ended. So a merge outlives a
// deploy and a crash of the daemon, its checks are never left running with
// nobody waiting for them, and after a reboot it is run again and skips
// what it had finished.
//
// What the daemon does at a job's end (this file) can be cut off too, and
// is then done again from the top. The task's own row is what makes that
// safe: a merge's end is acted on only while the row still says merging,
// and the first thing acting on it does is move the row off merging. Every
// path that calls a merge job settled therefore leaves the row on another
// status; one that could not write it returns an error and is tried again.

// Jobs is the job manager as the queue uses it (*jobs.Manager).
type Jobs interface {
	Start(jobs.New) (store.Job, error)
	Open(kinds ...string) ([]store.Job, error)
	OpenJob(subject string) (*store.Job, error)
	Unit(id string) string
	Steps(id string) ([]jobs.Entry, error)
	Applied(id string) (*jobs.Journal, error)
}

// The kinds of job the queue starts. MergeKind's Runner is RunLand,
// TidyKind's RunTidy; their Handlers are the runner's (MergeHandler,
// TidyHandler).
const (
	MergeKind = "task-merge"
	TidyKind  = "task-tidy"
)

const (
	// mergeLimit is how long one run of a merge may take: far past any real
	// one, short of the service's own backstop.
	mergeLimit = 3 * time.Hour
	// perCheckLine is how long one line of a repo's checks may run, when
	// the runner does not say (Runner.CheckLine).
	perCheckLine = 20 * time.Minute
	// askFormat is the format of what the daemon keeps of a job on its row
	// (mergeAsk, tidyAsk). A daemon that finds another one does not read it.
	askFormat = 1
)

// mergeSubject is whose job a feature's merge is. One open job per subject
// is the registry's own rule, so a feature cannot be merged twice at once.
func mergeSubject(taskID int64) string { return "task:" + strconv.FormatInt(taskID, 10) }

// taskOfSubject reads the task's number back out of either kind's subject.
func taskOfSubject(subject string) (int64, bool) {
	_, num, ok := strings.Cut(subject, ":")
	id, err := strconv.ParseInt(num, 10, 64)
	return id, ok && err == nil
}

// mergeAsk is what the daemon keeps of a merge it started, on the job's
// row: the feature's parts as the merge found them, in the order their
// branches were handed to the landing. The tidy-up still has to know which
// worktrees there were when the rows are gone (a merged task can be removed
// from the list while its tidy-up runs).
type mergeAsk struct {
	Format int       `json:"format"`
	TaskID int64     `json:"taskId"`
	Parts  []askPart `json:"parts"`
}

// askPart is one part as the merge found it.
type askPart struct {
	ID       int64  `json:"id"`
	RepoPath string `json:"repoPath"`
	Worktree string `json:"worktree"`
	Branch   string `json:"branch"`
}

func (p askPart) work(taskID int64) Work {
	return Work{RepoPath: p.RepoPath, Worktree: p.Worktree, Branch: p.Branch, Label: fmt.Sprintf("#%d", taskID)}
}

func askOf(j mergeJob) mergeAsk {
	ask := mergeAsk{Format: askFormat, TaskID: j.task.ID}
	for _, p := range j.parts {
		ask.Parts = append(ask.Parts, askPart{ID: p.ID, RepoPath: p.RepoPath, Worktree: p.Worktree, Branch: p.Branch})
	}
	return ask
}

// readAsk reads what the daemon kept of a job on its row.
func readAsk[T any](job store.Job, format func(T) int) (T, error) {
	var ask T
	if err := json.Unmarshal([]byte(job.Ctx), &ask); err != nil {
		return ask, fmt.Errorf("what was kept of the job is not readable: %w", err)
	}
	if got := format(ask); got != askFormat {
		return ask, fmt.Errorf("what was kept of the job is in format %d and this ccmuxd reads format %d", got, askFormat)
	}
	return ask, nil
}

// startMerge starts the job of a feature pickMerge claimed. It runs outside
// the lock, like a part's start: starting a service takes a moment. A job
// that does not start is said on the task, for a person: put back in line
// it would be tried again on every pass, each try leaving a job's row.
func (r *Runner) startMerge(j mergeJob) {
	perLine := r.CheckLine
	if perLine <= 0 {
		perLine = perCheckLine
	}
	_, err := r.Jobs.Start(jobs.New{
		Kind: MergeKind, Subject: mergeSubject(j.task.ID), Name: fmt.Sprintf("merge-task-%d", j.task.ID),
		Input: LandInput{Work: worksOf(j.parts), Tag: "tasks", PerLineSeconds: int(perLine.Seconds())},
		Ctx:   askOf(j), Limit: mergeLimit,
	})
	switch {
	case err == nil:
		return
	case errors.Is(err, store.ErrJobOpen):
		// A merge job of this feature runs already; its end settles the row.
		log.Printf("tasks: #%d: a merge job of it is open already, no second one is started", j.task.ID)
		return
	}
	log.Printf("tasks: #%d merge did not start: %v", j.task.ID, err)
	if err := r.failForPerson(j.task.ID, "the merge did not start: "+err.Error()); err != nil {
		log.Printf("tasks: #%d: that its merge did not start is not on its row; it is put back in line on the next pass: %v", j.task.ID, err)
	}
}

// recoverMerges is the start of the daemon as the merge line sees it, and
// every pass after. A task whose merge job has not ended is left exactly as
// it is: the job manager watches the job and hands its end to MergeHandler,
// whether it ran through a restart or has to be run again. Only a task
// that says merging and has no open job is put back in line: its job was
// never started (the daemon stopped between claiming it and starting it),
// or it was being merged by a daemon from before merges were jobs. open is
// the open merge jobs, read this pass. Under the lock.
func (r *Runner) recoverMerges(tasks []store.Task, open []store.Job) bool {
	changed := false
	for i, t := range tasks {
		if t.Status != Merging || hasSubject(open, mergeSubject(t.ID)) {
			continue
		}
		log.Printf("tasks: #%d says merging and has no merge job: it is put back in line", t.ID)
		t.Status, t.Note = ReadyToMerge, ""
		if r.saveTask(t) {
			tasks[i] = t
			changed = true
		}
	}
	return changed
}

func hasSubject(open []store.Job, subject string) bool {
	for _, j := range open {
		if j.Subject == subject {
			return true
		}
	}
	return false
}

// noteNoJobs is a built feature's note on a daemon that runs no jobs.
const noteNoJobs = "waiting to merge: this daemon runs no jobs (%s), so nothing is merged here; merge it by hand, then Mark merged"

// waitForJobs says on every built feature why it does not merge on a
// daemon that runs no jobs. A task that says merging is left as it is:
// whether a job of it still runs cannot be asked here. Under the lock.
func (r *Runner) waitForJobs(tasks []store.Task) bool {
	changed := false
	for i, t := range tasks {
		if t.Status == ReadyToMerge {
			changed = r.noteWait(tasks, i, fmt.Sprintf(noteNoJobs, r.JobsOff)) || changed
		}
	}
	return changed
}

// whileMerging changes task id's row only while it still says merging, and
// reports whether it did. That is the guard that lets a job's end be acted
// on more than once: the first time moves the row off merging, and every
// time after finds nothing to do. A row that cannot be read or written is
// an error, never "nothing to do". Under the lock.
func (r *Runner) whileMerging(id int64, change func(store.Task, []store.Part) store.Task) (bool, error) {
	tasks, err := r.Store.Tasks()
	if err != nil {
		return false, err
	}
	parts, err := r.Store.Parts()
	if err != nil {
		return false, err
	}
	for _, t := range tasks {
		if t.ID != id || t.Status != Merging {
			continue
		}
		next := change(t, PartsOf(parts, id))
		next.UpdatedAt = r.Now().UnixMilli()
		found, err := r.Store.SaveTask(next)
		if err != nil {
			return false, fmt.Errorf("task #%d not saved (%s): %w", id, next.Status, err)
		}
		return found, nil
	}
	return false, nil
}

// failForPerson settles a merge that ended in a way nobody's worker can
// fix: the task reads merge failed, with why, and waits for a person.
func (r *Runner) failForPerson(taskID int64, why string) error {
	r.Lock.Lock()
	was, err := r.whileMerging(taskID, func(t store.Task, _ []store.Part) store.Task {
		t.Status, t.Note, t.FixPart = MergeFailed, noteOf(errors.New(why)), 0
		return t
	})
	r.Lock.Unlock()
	r.changed(was)
	return err
}

// MergeHandler is the daemon's side of a feature's merge job.
func (r *Runner) MergeHandler() jobs.Handler { return mergeHandler{r} }

type mergeHandler struct{ r *Runner }

// Apply acts on a merge's result. Failed: the failure goes where it can be
// fixed (settleFailed: back to a worker, back in line behind a person's
// folder, or to a person). Landed: the task is saved done, its workers are
// closed, and the tidy-up job takes the worktrees away.
func (h mergeHandler) Apply(job store.Job, res jobs.Result) error {
	ask, err := readAsk(job, func(a mergeAsk) int { return a.Format })
	if err != nil {
		return h.r.unread(job, err)
	}
	out, err := ReadLandOutput(res)
	if err != nil {
		return h.r.unread(job, err)
	}
	if failure := out.Err(); failure != nil {
		log.Printf("tasks: #%d merge failed: %v", ask.TaskID, failure)
		return h.r.mergeFailed(job, ask, failure)
	}
	log.Printf("tasks: #%d %s", ask.TaskID, out.Note)
	return h.r.mergeLanded(job, ask, out.Note)
}

// Lost settles a merge that ended with no result the daemon reads: the
// task waits for a person, and its note says what the job's journal knows
// had landed, because a merge that died between two trunks is half in. A
// result that is there and cannot be read is another matter: that merge may
// have landed whole, and nothing is said about it but that (unread).
func (h mergeHandler) Lost(job store.Job, cause error) error {
	if jobs.Unread(cause) {
		return h.r.unread(job, cause)
	}
	return h.r.cutShort(job, "its merge did not finish: "+cause.Error())
}

// Stopped settles a merge whose job was stopped on request. Nothing in
// ccmux asks for that today; a job stopped by hand ends here.
func (h mergeHandler) Stopped(job store.Job) error {
	return h.r.cutShort(job, "its merge was stopped")
}

// cutShort settles a merge that left no result.
func (r *Runner) cutShort(job store.Job, why string) error {
	id, ok := taskOfSubject(job.Subject)
	if !ok {
		log.Printf("tasks: merge job %s names no task (%q); nothing to settle", job.ID, job.Subject)
		return nil
	}
	steps, err := r.Jobs.Steps(job.ID)
	landed := "Nothing had landed."
	switch got := Landed(steps); {
	case err != nil:
		landed = "What had landed could not be read (" + err.Error() + "): look at each repo's trunk."
	case len(got) > 0:
		landed = "Already merged, and staying so: " + strings.Join(got, ", ") + "."
	}
	log.Printf("tasks: #%d: %s. %s", id, why, landed)
	return r.failForPerson(id, fmt.Sprintf("%s. %s\nIts log: journalctl --user -u %s\nPut it back to merge to try again (what already landed is skipped), or Mark merged.", why, landed, r.Jobs.Unit(job.ID)))
}

// unread settles a merge job this daemon cannot read (what it kept of it,
// or its result, is in another format: the job was started by another
// version). It guesses nothing about what landed.
func (r *Runner) unread(job store.Job, cause error) error {
	log.Printf("tasks: MERGE JOB %s NOT READ: %v; nothing is assumed about what landed", job.ID, cause)
	id, ok := taskOfSubject(job.Subject)
	if !ok {
		return nil
	}
	return r.failForPerson(id, fmt.Sprintf("its merge ended, and this ccmuxd cannot read how (%v). It may have landed, or half of it.\n"+
		"Look at each repo's trunk and at journalctl --user -u %s, then Mark merged or put it back to merge.", cause, r.Jobs.Unit(job.ID)))
}

// mergeFailed sends a failed merge where it can be fixed.
func (r *Runner) mergeFailed(job store.Job, ask mergeAsk, failure error) error {
	r.Lock.Lock()
	was, err := r.whileMerging(ask.TaskID, func(t store.Task, parts []store.Part) store.Task {
		return r.settleFailed(t, forWorker(failure, ask, parts), job.ID)
	})
	r.Lock.Unlock()
	r.changed(was)
	return err
}

// forWorker turns a failure inside one piece of work's own worktree back
// into one that names the part whose worker can fix it, as the part is now.
// A part that is gone since leaves the failure for a person.
func forWorker(failure error, ask mergeAsk, parts []store.Part) error {
	i, ok := Fixable(failure)
	if !ok || i >= len(ask.Parts) {
		return failure
	}
	for _, p := range parts {
		if p.ID == ask.Parts[i].ID {
			return workerFixable{part: p, err: failure}
		}
	}
	return failure
}

// mergeLanded settles a merge that landed. The task is saved done before
// its tidy-up, which can take minutes (a worktree with a node_modules in
// it): work that is in the trunks is marked as such at once. The note says
// the tidy-up is not finished until the tidy-up's own end says otherwise.
func (r *Runner) mergeLanded(job store.Job, ask mergeAsk, note string) error {
	r.Lock.Lock()
	was, err := r.whileMerging(ask.TaskID, func(t store.Task, _ []store.Part) store.Task {
		t.Status, t.Note, t.FinishedAt = Done, note+noteTidying, r.Now().UnixMilli()
		return t
	})
	r.Lock.Unlock()
	if err != nil {
		return err
	}
	r.changed(was)
	// Not only when the row was merging just now: a daemon cut off right
	// after that save comes back here with the row already done, and the
	// tidy-up still to start.
	return r.tidy(job, ask)
}

// ReadLandOutput reads a landing job's result as a landing's end. A job
// that could not do its work at all (its error) is a failed landing in
// nobody's worktree in particular.
func ReadLandOutput(res jobs.Result) (LandOutput, error) {
	if res.Error != "" {
		return LandOutput{Error: res.Error, Fixable: -1}, nil
	}
	var out LandOutput
	if err := json.Unmarshal(res.Output, &out); err != nil {
		return out, fmt.Errorf("the merge's result is not readable: %w", err)
	}
	if !out.OK && out.Error == "" {
		return out, errors.New("the merge's result says neither that it landed nor why not")
	}
	return out, nil
}
