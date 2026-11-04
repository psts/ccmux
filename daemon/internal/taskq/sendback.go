package taskq

import (
	"errors"
	"fmt"
	"log"
	"path/filepath"
	"time"

	"ccmux.dev/ccmuxd/internal/store"
)

// A failed merge is one of three kinds, and each goes where it can be fixed:
//
//   - Inside one part's own worktree (its branch clashes with the trunk, the
//     checks failed on it, it left work uncommitted, it changed while the
//     checks ran): its worker knows the code, and its pane stays open until
//     the merge, so the failure goes back to it. Once on its own
//     (AutoFixes), after that by a person's "Send back to worker".
//   - In a repo's main folder (uncommitted changes on the trunk, the trunk
//     checked out elsewhere, git busy or a stale lock): that is a person's
//     work in progress, which nothing touches. The feature waits, the way it
//     waits before a merge, and merges once the folder is clean.
//   - Anything else (gate files changed, no checks, half merged, git broken
//     mid-rebase): it waits for a person, as before.

// maxAutoFixes is how many times a failed merge goes back to a worker on its
// own. One: a second failure of the same feature asks a person.
const maxAutoFixes = 1

// workerFixable is a merge failure inside one part's own worktree.
type workerFixable struct {
	part store.Part
	err  error
}

func (e workerFixable) Error() string { return e.err.Error() }
func (e workerFixable) Unwrap() error { return e.err }

// folderBlocked is a merge stopped by a repo's main folder after its checks;
// why is mergeBlocked's "waiting to merge: ..." line.
type folderBlocked struct{ why string }

func (e folderBlocked) Error() string { return e.why }

// rebaseClash is a branch that does not rebase onto its trunk, the rebase
// undone cleanly: its worker can rebase it by hand.
type rebaseClash struct {
	trunk string
	err   error
}

func (e rebaseClash) Error() string {
	return fmt.Sprintf("its branch clashes with %s: %v", e.trunk, e.err)
}
func (e rebaseClash) Unwrap() error { return e.err }

const noteSentBack = "sent back to its worker after a failed merge"

// SendBack is the rule for returning a built part to its worker after its
// feature failed to merge: the part runs again, from now, in the same pane,
// and so does the feature. Its other parts stay built.
func SendBack(t store.Task, p store.Part, now time.Time) (store.Task, store.Part, error) {
	switch {
	case t.Status != MergeFailed:
		return t, p, fmt.Errorf("this task is %s; only one that failed to merge goes back to a worker", t.Status)
	case p.Status != Ready:
		return t, p, fmt.Errorf("this part is %s; only a built part goes back to its worker", p.Status)
	case p.PaneID == "":
		return t, p, errors.New("this part has no worker to send it back to")
	}
	p.Status, p.Note, p.StartedAt, p.FinishedAt = Running, noteSentBack, now.UnixMilli(), 0
	t.Status, t.FinishedAt = Running, 0
	return t, p, nil
}

// SendBackPart is a person's "Send back to worker": part partID of task
// taskID goes back to its worker with the task's failure note.
func (r *Runner) SendBackPart(taskID, partID int64) error {
	r.Lock.Lock()
	defer r.Lock.Unlock()
	t, p, err := r.taskAndPart(taskID, partID)
	if err != nil {
		return err
	}
	if _, err := r.sendBack(t, p, t.Note); err != nil {
		return err
	}
	r.Host.TasksChanged()
	return nil
}

func (r *Runner) taskAndPart(taskID, partID int64) (store.Task, store.Part, error) {
	tasks, err := r.Store.Tasks()
	if err != nil {
		return store.Task{}, store.Part{}, err
	}
	parts, err := r.Store.Parts()
	if err != nil {
		return store.Task{}, store.Part{}, err
	}
	for _, t := range tasks {
		if t.ID != taskID {
			continue
		}
		for _, p := range PartsOf(parts, taskID) {
			if p.ID == partID {
				return t, p, nil
			}
		}
	}
	return store.Task{}, store.Part{}, fmt.Errorf("task #%d has no part %d", taskID, partID)
}

// sendBack tells part p's worker why the merge failed and saves the part
// and its task running again. The worker hears first: a part saved running
// that nobody told would only sit until the watcher flags it. The part and
// the task are saved in one step: a part running again under a task still
// failed would have its fix accepted and never merged. A save that fails is
// an error, never a quiet log: nothing changed, and the worker's report will
// bounce until it is sent back again. Under the lock.
func (r *Runner) sendBack(t store.Task, p store.Part, why string) (store.Task, error) {
	nt, np, err := SendBack(t, p, r.Now())
	if err != nil {
		return t, err
	}
	trunk, err := r.Git.Trunk(p.RepoPath)
	if err != nil {
		return t, fmt.Errorf("reading %s's trunk: %w", filepath.Base(p.RepoPath), err)
	}
	if err := r.Host.TellWorker(p.PaneID, sendBackText(t.ID, trunk, why)); err != nil {
		return t, fmt.Errorf("its worker did not hear it: %w", err)
	}
	nt.Note = noteOf(fmt.Errorf("sent back to its worker in %s after a failed merge:\n%s",
		filepath.Base(p.RepoPath), why))
	saveAt := r.Now().UnixMilli()
	nt.UpdatedAt, np.UpdatedAt = saveAt, saveAt
	if ok, err := r.Store.SaveTaskAndPart(nt, np); err != nil || !ok {
		log.Printf("tasks: #%d sent back to its worker but not saved (found %v): %v", t.ID, ok, err)
		return t, errors.New("its worker was told, but nothing was saved, so the queue will refuse its report; send it back again")
	}
	return nt, nil
}

// sendBackText is what the worker reads: why, and what it may do about it
// that its brief otherwise forbids.
func sendBackText(taskID int64, trunk, why string) string {
	return fmt.Sprintf(`[ccmux] Task #%d: the merge of your part failed, so it is back with you.

%s

The merge rebases your branch onto %s before it runs the checks, so your
branch may already sit on top of %s (git log shows it). Fix this in your
worktree. For this fix you may rebase onto %s, the one exception to your
brief. Commit, run the repo's checks, then call the tasks tool with action
"report", status "done" again. If you cannot fix it, report "stuck" with
what you need.`, taskID, why, trunk, trunk, trunk)
}

// settleFailed decides where a failed merge goes, by its kind. Under the
// lock; t is the task as it was claimed for the merge.
func (r *Runner) settleFailed(t store.Task, err error) store.Task {
	var blocked folderBlocked
	if errors.As(err, &blocked) {
		t.Status, t.Note = ReadyToMerge, blocked.why
		return t
	}
	t.Status, t.Note = MergeFailed, noteOf(err)
	var fix workerFixable
	if !errors.As(err, &fix) || t.AutoFixes >= maxAutoFixes {
		return t
	}
	t.AutoFixes++
	sent, sendErr := r.sendBack(t, fix.part, t.Note)
	if sendErr != nil {
		t.Note = noteOf(errors.New(t.Note + "\n(the send-back to its worker failed: " + sendErr.Error() + ")"))
		return t
	}
	return sent
}
