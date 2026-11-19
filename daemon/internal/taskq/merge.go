package taskq

import (
	"fmt"
	"strings"
	"unicode/utf8"

	"ccmux.dev/ccmuxd/internal/store"
)

// A feature whose every part is built is merged as one: each part's branch
// is rebased onto its repo's trunk and checked there, and only when all of
// them are green does each trunk move forward, a fast-forward only. Nothing
// is pushed. One merge runs at a time, as a job in its own service
// (mergejob.go), so a long check never holds up the runner's other work and
// does not end with the daemon.
//
// This file is the queue's side of picking a merge: which feature merges
// when, and what its task row says while it waits. The merge itself (the
// git and the checks) is land.go, which a thread with helpers lands through
// too; what is done with its end is mergejob.go and sendback.go.

// mergeJob is a feature being merged, with its parts.
type mergeJob struct {
	task  store.Task
	parts []store.Part
}

const maxNote = 2000

// pickMerge claims the first built feature that can merge now, walking
// tasks in order (ByPriority); one that has to wait says why in its note
// (mergeWait). Under the lock.
func (r *Runner) pickMerge(tasks []store.Task, parts []store.Part, order []int) (*mergeJob, bool) {
	changed := false
	for _, i := range order {
		t := tasks[i]
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

// lander is the daemon's own look at a repo before a merge (whether its
// folder can take one). The landing itself runs in the merge's job, with a
// Lander of its own (RunLand).
func (r *Runner) lander() Lander {
	return Lander{Git: r.Git, MergeGit: r.MergeGit, Tag: "tasks"}
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

// noteTidying ends a merged feature's note while its tidy-up runs. The
// tidy-up's own end takes it away again (endTidy), or says after it why it
// did not finish: the one sign that a worktree, a branch or a worker may
// still be around.
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
