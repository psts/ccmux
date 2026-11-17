package taskq

import (
	"errors"
	"fmt"
	"log"
	"strings"
	"unicode/utf8"

	"ccmux.dev/ccmuxd/internal/store"
)

// A feature whose every part is built is merged as one: each part's branch
// is rebased onto its repo's trunk and checked there, and only when all of
// them are green does each trunk move forward, a fast-forward only. Nothing
// is pushed. One merge runs at a time, in its own goroutine, so a long
// check never holds up the runner's other work.
//
// This file is the queue's side of a merge: which feature merges when, what
// its task row says, where a failure goes. The merge itself (the git and
// the checks) is land.go, which a thread with helpers lands through too.

// mergeJob is a feature being merged, with its parts.
type mergeJob struct {
	task  store.Task
	parts []store.Part
}

const maxNote = 2000

// recoverMerges puts a task the runner was merging when the daemon stopped
// back to be merged again. Called under the lock, with no merge in flight.
func (r *Runner) recoverMerges(tasks []store.Task) bool {
	changed := false
	for i, t := range tasks {
		if t.Status != Merging {
			continue
		}
		t.Status, t.Note = ReadyToMerge, ""
		if r.saveTask(t) {
			tasks[i] = t
			changed = true
		}
	}
	return changed
}

// pickMerge claims the first built feature that can merge now; one that
// has to wait says why in its note (mergeWait). Under the lock.
func (r *Runner) pickMerge(tasks []store.Task, parts []store.Part) (*mergeJob, bool) {
	changed := false
	for i, t := range tasks {
		if t.Status != ReadyToMerge {
			continue
		}
		mine := PartsOf(parts, t.ID)
		if why := r.mergeWait(t.ID, mine); why != "" {
			changed = r.noteWait(tasks, i, why) || changed
			continue
		}
		t.Status, t.Note = Merging, ""
		if !r.saveTask(t) {
			continue
		}
		tasks[i] = t
		r.merging = true
		return &mergeJob{task: t, parts: mine}, true
	}
	return nil, changed
}

// mergeWait says why built feature id cannot merge this pass, "" when it
// can. A wait on a person's own folder parks it: it gives its slot up
// until the folder is clean. A wait on its own worker's turn to end
// (Settled) does not: that is a minute or so, and the slot is what keeps
// the next build off the checks about to run. Under the lock.
func (r *Runner) mergeWait(id int64, mine []store.Part) string {
	if why := r.mergeBlocked(mine); why != "" {
		r.park(id)
		return why
	}
	delete(r.parked, id)
	now := r.Now()
	for _, p := range mine {
		st, ok := r.Host.TaskPane(p.PaneID)
		if !Settled(p, st, ok, now) {
			return noteSettling
		}
	}
	return ""
}

// noteWait saves why tasks[i] waits to merge, when that changed.
func (r *Runner) noteWait(tasks []store.Task, i int, why string) bool {
	t := tasks[i]
	if t.Note == why {
		return false
	}
	t.Note = why
	if !r.saveTask(t) {
		return false
	}
	tasks[i] = t
	return true
}

// park records that built feature id waits on a person's folder, so it
// holds no slot meanwhile. Under the lock.
func (r *Runner) park(id int64) {
	if r.parked == nil {
		r.parked = map[int64]bool{}
	}
	r.parked[id] = true
}

// forgetParked drops the folder waits of features no longer in line to
// merge (merged by hand, cancelled, sent back): one built again later holds
// its slot until pickMerge has looked at its folder afresh. Under the lock.
func (r *Runner) forgetParked(tasks []store.Task) {
	if len(r.parked) == 0 {
		return
	}
	waiting := map[int64]bool{}
	for _, t := range tasks {
		if t.Status == ReadyToMerge && r.parked[t.ID] {
			waiting[t.ID] = true
		}
	}
	r.parked = waiting
}

// lander is the merge itself, over this runner's git and checks (land.go).
func (r *Runner) lander() Lander {
	return Lander{Git: r.Git, MergeGit: r.MergeGit, Checks: r.Checks, Tag: "tasks"}
}

// workOf is a part as the merge sees it: its branch, where it is checked
// out, and its task's number for the log.
func workOf(p store.Part) Work {
	return Work{RepoPath: p.RepoPath, Worktree: p.Worktree, Branch: p.Branch, Label: fmt.Sprintf("#%d", p.TaskID)}
}

func worksOf(parts []store.Part) []Work {
	out := make([]Work, len(parts))
	for i, p := range parts {
		out[i] = workOf(p)
	}
	return out
}

// mergeBlocked says why the feature cannot merge now, "" when it can.
func (r *Runner) mergeBlocked(parts []store.Part) string {
	return r.lander().Blocked(worksOf(parts))
}

// merge runs one feature's merge and records how it went. A merged feature
// is saved done before its tidy-up, which can take minutes (a worktree with
// a node_modules in it): a daemon stopped meanwhile then finds work that is
// in the trunks marked as such, not a merge to run again against a folder
// half removed. The merge line stays taken until the tidy-up ends. The
// note says the tidy-up is not finished until it is, then what it had to
// leave; that last save always runs, so a first one that failed gets a
// second go.
func (r *Runner) merge(j mergeJob) {
	note, err := r.mergeParts(j.parts)
	if err != nil {
		log.Printf("tasks: #%d merge failed: %v", j.task.ID, err)
		r.Lock.Lock()
		r.merging = false
		saved := r.saveTask(r.settleFailed(j.task, err))
		r.Lock.Unlock()
		r.changed(saved)
		return
	}
	done := j.task
	done.Status, done.Note, done.FinishedAt = Done, note+noteTidying, r.Now().UnixMilli()
	log.Printf("tasks: #%d %s", done.ID, note)
	r.Lock.Lock()
	saved := r.saveTask(done)
	r.Lock.Unlock()
	r.changed(saved)

	done.Note = note + r.cleanUp(j)
	r.Lock.Lock()
	r.merging = false
	saved = r.saveTask(done)
	r.Lock.Unlock()
	r.changed(saved)
}

// noteTidying ends a merged feature's note while its tidy-up runs. A
// tidy-up cut short (the daemon stopped) leaves it there, the one sign that
// a worktree, a branch or a worker may still be around: nothing takes a
// tidy-up up again.
const noteTidying = "; tidy-up not finished"

// changed tells the lenses the queue changed, when something was saved.
func (r *Runner) changed(saved bool) {
	if saved {
		r.Host.TasksChanged()
	}
}

// noteOf is a failed merge's note: its first line whole (the repo and what
// failed), then as much of the end (the check output's last lines) as fits.
func noteOf(err error) string {
	msg := err.Error()
	head, rest, found := strings.Cut(msg, "\n")
	if !found || len(msg) <= maxNote {
		return cut(msg, maxNote)
	}
	head = keepStart(head, maxNote/4)
	return head + "\n" + cut(rest, maxNote-len(head)-1)
}

// mergeParts lands every part of a feature, or none (Lander.Land). A
// failure inside one part's own worktree comes back naming that part, so it
// can go back to its worker (sendback.go).
func (r *Runner) mergeParts(parts []store.Part) (string, error) {
	note, err := r.lander().Land(worksOf(parts))
	var fix fixable
	if errors.As(err, &fix) {
		return "", workerFixable{part: parts[fix.index], err: err}
	}
	return note, err
}

// cleanUp closes a merged feature's workers and removes their worktrees,
// branches and briefs. Nothing here is forced: a worktree whose worker did
// not close, or that holds anything uncommitted, stays, and so does a branch
// with a commit the trunk lacks. The returned text names what stayed, for
// the done note; the merged work is in the trunks either way.
func (r *Runner) cleanUp(j mergeJob) string {
	var left []string
	for _, p := range j.parts {
		if p.Worktree == "" {
			continue
		}
		if why := r.tidyPart(p); why != "" {
			log.Printf("tasks: #%d merged; %s", j.task.ID, why)
			left = append(left, why)
		}
	}
	if len(left) == 0 {
		return ""
	}
	return "; left for you: " + strings.Join(left, "; ")
}

// tidyPart closes one part's worker and removes its worktree, branch, brief
// and any check output, saying what it had to leave ("" when nothing).
func (r *Runner) tidyPart(p store.Part) string {
	if err := r.Host.ClearTaskFolder(p.Worktree); err != nil {
		return fmt.Sprintf("%s (its worker did not close: %v)", p.Worktree, err)
	}
	return r.lander().Remove(workOf(p), p.Worktree+".md")
}

// keepStart keeps the first n bytes of s, ending on a whole character.
func keepStart(s string, n int) string {
	if len(s) <= n {
		return s
	}
	end := n
	for end > 0 && !utf8.RuneStart(s[end]) {
		end--
	}
	return s[:end] + "…"
}

// cut keeps the last n bytes of s, starting on a whole character.
func cut(s string, n int) string {
	if len(s) <= n {
		return s
	}
	start := len(s) - n
	for start < len(s) && !utf8.RuneStart(s[start]) {
		start++
	}
	return "…" + s[start:]
}
