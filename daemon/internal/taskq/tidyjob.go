package taskq

import (
	"encoding/json"
	"fmt"
	"log"
	"path/filepath"
	"strings"
	"time"

	"ccmux.dev/ccmuxd/internal/jobs"
	"ccmux.dev/ccmuxd/internal/store"
)

// The tidy-up after a merge is a job of its own. The daemon closes the
// merged feature's workers (only it can: they are its panes), then the job
// removes their worktrees, branches, briefs and check output, which can
// take minutes. Nothing here is forced: a worktree whose worker did not
// close, or that holds anything uncommitted, stays, and so does a branch
// with a commit the trunk lacks. The task's note says the tidy-up is not
// finished until the job's end is acted on, and then what had to stay.
//
// Every step can be repeated: closing a worker that is gone closes nothing,
// and removing a worktree that is gone finishes what is left of it (its
// branch). So a job that is killed is simply run again.

// tidyLimit is how long one run of a tidy-up may take.
const tidyLimit = time.Hour

// tidySubject is whose job a feature's tidy-up is. Its own subject, not the
// merge's: it starts while the merge's job is still being settled.
func tidySubject(taskID int64) string { return "task-tidy:" + fmt.Sprint(taskID) }

// tidyAsk is what the daemon keeps of a tidy-up on its job's row: what the
// daemon itself already had to leave (a worker that did not close).
type tidyAsk struct {
	Format int      `json:"format"`
	TaskID int64    `json:"taskId"`
	Left   []string `json:"left,omitempty"`
}

// TidyInput is what a tidy-up job is started from: the landed work whose
// workers are closed.
type TidyInput struct {
	Work []Work `json:"work"`
	Tag  string `json:"tag"`
}

// TidyOutput is how a tidy-up went: what it had to leave, each with why.
type TidyOutput struct {
	Left []string `json:"left,omitempty"`
}

// RunTidy is the tidy-up job's Runner. It holds the lock of every repo it
// works in, so it never removes a worktree or a branch while a landing job
// rebases or moves a trunk in the same repo.
func RunTidy(r *jobs.Run) (any, error) {
	var in TidyInput
	if err := json.Unmarshal(r.Spec.Input, &in); err != nil {
		return nil, fmt.Errorf("the tidy-up's input is not readable: %w", err)
	}
	repos := make([]string, len(in.Work))
	for i, w := range in.Work {
		repos[i] = w.RepoPath
	}
	if err := r.LockRepos(repos); err != nil {
		return nil, err
	}
	defer r.Unlock()
	git := ExecGit{}
	return tidyWork(Lander{Git: git, MergeGit: git, Tag: in.Tag}, in.Work, runSteps{r}, r.Ctx.Err)
}

// tidyWork removes each piece of landed work that an earlier run has not
// removed already, and returns what had to stay. Only a worktree that is
// really gone is written down as done: one that stayed is tried again by a
// later run, which costs nothing and may find it clean by then. halt is
// asked before each one; its error ends the run there.
func tidyWork(l Lander, work []Work, steps Steps, halt func() error) (TidyOutput, error) {
	var out TidyOutput
	for _, w := range work {
		key := "removed:" + w.Worktree
		if steps.Done(key, nil) {
			continue
		}
		if err := halt(); err != nil {
			return out, err
		}
		steps.Begin(key, filepath.Base(w.RepoPath)+": removing its worktree")
		if why := l.Remove(w, w.Worktree+".md", RebaseMark(w.Worktree)); why != "" {
			log.Printf("%s: %s merged; %s", l.Tag, w.Label, why)
			out.Left = append(out.Left, why)
			continue
		}
		steps.Finished(key, nil)
	}
	return out, nil
}

// tidy closes a merged feature's workers and starts the job that removes
// their worktrees. It is called again when the daemon was cut off anywhere
// in it, so it starts no second job: one that is written down as started,
// or still open, is enough. A job that does not start is said on the task
// and not tried again: its worktrees are then a person's.
func (r *Runner) tidy(job store.Job, ask mergeAsk) error {
	steps, err := r.Jobs.Applied(job.ID)
	if err != nil {
		return fmt.Errorf("task #%d: the record of what was done with its merge: %w", ask.TaskID, err)
	}
	if steps.Done("tidy", nil) {
		return nil
	}
	open, err := r.Jobs.OpenJob(tidySubject(ask.TaskID))
	if err != nil {
		return fmt.Errorf("task #%d: whether its tidy-up runs already could not be read: %w", ask.TaskID, err)
	}
	if open == nil {
		if err := r.startTidy(ask); err != nil {
			return err
		}
	}
	if err := steps.Record("tidy", nil); err != nil {
		log.Printf("tasks: #%d: that its tidy-up was started is not written down, so it may be started again: %v", ask.TaskID, err)
	}
	return nil
}

// startTidy closes the workers and starts the tidy-up job for the worktrees
// whose worker closed. With nothing to remove, or a job that does not
// start, the task's note is ended here.
func (r *Runner) startTidy(ask mergeAsk) error {
	work, left := r.closeWorkers(ask)
	if len(work) == 0 {
		return r.endTidy(ask.TaskID, func(merged string) string { return merged + leftNote(left) })
	}
	_, err := r.Jobs.Start(jobs.New{
		Kind: TidyKind, Subject: tidySubject(ask.TaskID), Name: fmt.Sprintf("tidy-task-%d", ask.TaskID),
		Input: TidyInput{Work: work, Tag: "tasks"},
		Ctx:   tidyAsk{Format: askFormat, TaskID: ask.TaskID, Left: left},
		Limit: tidyLimit,
	})
	if err == nil {
		return nil
	}
	log.Printf("tasks: #%d merged; its tidy-up did not start: %v", ask.TaskID, err)
	for _, w := range work {
		left = append(left, w.Worktree)
	}
	return r.endTidy(ask.TaskID, func(merged string) string {
		return merged + noteTidying + ": it did not start (" + firstLine(err.Error()) + ")" + leftNote(left)
	})
}

// closeWorkers closes the worker of every part that has a worktree, and
// returns the work whose worker closed and what it had to leave.
func (r *Runner) closeWorkers(ask mergeAsk) (work []Work, left []string) {
	for _, p := range ask.Parts {
		if p.Worktree == "" {
			continue
		}
		if err := r.Host.ClearTaskFolder(p.Worktree); err != nil {
			why := fmt.Sprintf("%s (its worker did not close: %v)", p.Worktree, err)
			log.Printf("tasks: #%d merged; %s", ask.TaskID, why)
			left = append(left, why)
			continue
		}
		work = append(work, p.work(ask.TaskID))
	}
	return work, left
}

// leftNote ends a merged feature's note with what its tidy-up left.
func leftNote(left []string) string {
	if len(left) == 0 {
		return ""
	}
	return "; left for you: " + strings.Join(left, "; ")
}

func firstLine(text string) string {
	line, _, _ := strings.Cut(text, "\n")
	return line
}

// endTidy ends a merged task's note once its tidy-up is over: end is given
// the note as the merge left it ("merged app into main") and returns what
// it reads from now on. Only a done task whose note still says the tidy-up
// is not finished is changed, so ending the same tidy-up twice changes
// nothing, and a task removed from the list meanwhile is nobody's to end.
func (r *Runner) endTidy(taskID int64, end func(merged string) string) error {
	r.Lock.Lock()
	saved, err := r.endTidyLocked(taskID, end)
	r.Lock.Unlock()
	r.changed(saved)
	return err
}

func (r *Runner) endTidyLocked(taskID int64, end func(merged string) string) (bool, error) {
	tasks, err := r.Store.Tasks()
	if err != nil {
		return false, err
	}
	for _, t := range tasks {
		if t.ID != taskID || t.Status != Done || !strings.HasSuffix(t.Note, noteTidying) {
			continue
		}
		t.Note, t.UpdatedAt = end(strings.TrimSuffix(t.Note, noteTidying)), r.Now().UnixMilli()
		found, err := r.Store.SaveTask(t)
		if err != nil {
			return false, fmt.Errorf("task #%d: the end of its tidy-up was not saved: %w", taskID, err)
		}
		return found, nil
	}
	return false, nil
}

// TidyHandler is the daemon's side of a feature's tidy-up job.
func (r *Runner) TidyHandler() jobs.Handler { return tidyHandler{r} }

type tidyHandler struct{ r *Runner }

// Apply ends the task's note with what the tidy-up left, the daemon's own
// leftovers first.
func (h tidyHandler) Apply(job store.Job, res jobs.Result) error {
	ask, err := readAsk(job, func(a tidyAsk) int { return a.Format })
	if err != nil {
		return h.unfinished(job, nil, err.Error())
	}
	if res.Error != "" {
		return h.unfinished(job, ask.Left, res.Error)
	}
	var out TidyOutput
	if err := json.Unmarshal(res.Output, &out); err != nil {
		return h.unfinished(job, ask.Left, "its result is not readable: "+err.Error())
	}
	left := append(ask.Left, out.Left...)
	return h.r.endTidy(ask.TaskID, func(merged string) string { return merged + leftNote(left) })
}

// Lost keeps the note saying the tidy-up is not finished, with why: a
// worktree, a branch or a brief may still be around.
func (h tidyHandler) Lost(job store.Job, cause error) error {
	ask, _ := readAsk(job, func(a tidyAsk) int { return a.Format })
	return h.unfinished(job, ask.Left, cause.Error())
}

func (h tidyHandler) Stopped(job store.Job) error {
	ask, _ := readAsk(job, func(a tidyAsk) int { return a.Format })
	return h.unfinished(job, ask.Left, "it was stopped")
}

// unfinished ends the note of a tidy-up that did not run to its end.
func (h tidyHandler) unfinished(job store.Job, left []string, why string) error {
	id, ok := taskOfSubject(job.Subject)
	if !ok {
		log.Printf("tasks: tidy-up job %s names no task (%q); nothing to settle", job.ID, job.Subject)
		return nil
	}
	log.Printf("tasks: #%d merged; its tidy-up did not finish: %s", id, why)
	return h.r.endTidy(id, func(merged string) string {
		return merged + noteTidying + ": " + firstLine(why) + " (journalctl --user -u " + h.r.Jobs.Unit(job.ID) + ")" + leftNote(left)
	})
}
