package taskq

import (
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"unicode/utf8"

	"ccmux.dev/ccmuxd/internal/store"
)

// A feature whose every part is built is merged as one: each part's branch
// is rebased onto its repo's trunk and checked there, and only when all of
// them are green does each trunk move forward, a fast-forward only. Nothing
// is pushed. One merge runs at a time, in its own goroutine, so a long
// check never holds up the runner's other work.

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

// pickMerge claims the first built feature whose repos' folders allow a
// merge now; one that has to wait says why in its note. Under the lock.
func (r *Runner) pickMerge(tasks []store.Task, parts []store.Part) (*mergeJob, bool) {
	changed := false
	for i, t := range tasks {
		if t.Status != ReadyToMerge {
			continue
		}
		mine := PartsOf(parts, t.ID)
		if why := r.mergeBlocked(mine); why != "" {
			if t.Note != why {
				t.Note = why
				changed = r.saveTask(t) || changed
				tasks[i] = t
			}
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

// mergeBlocked says why the feature cannot merge now, "" when it can.
func (r *Runner) mergeBlocked(parts []store.Part) string {
	for _, p := range parts {
		trunk, err := r.Git.Trunk(p.RepoPath)
		if err != nil {
			return "waiting: " + err.Error()
		}
		why, err := r.MergeGit.TrunkBlocked(p.RepoPath, trunk)
		if err != nil {
			return "waiting: " + err.Error()
		}
		if why != "" {
			return "waiting to merge: " + why
		}
	}
	return ""
}

// merge runs one feature's merge, tidies up after a success, and records
// how it went: a done note also names anything the tidy-up had to leave.
func (r *Runner) merge(j mergeJob) {
	note, err := r.mergeParts(j.parts)
	if err == nil {
		note += r.cleanUp(j)
	}
	r.Lock.Lock()
	r.merging = false
	t := j.task
	if err != nil {
		log.Printf("tasks: #%d merge failed: %v", t.ID, err)
		t = r.settleFailed(t, err)
	} else {
		t.Status, t.Note, t.FinishedAt = Done, note, r.Now().UnixMilli()
		log.Printf("tasks: #%d %s", t.ID, note)
	}
	saved := r.saveTask(t)
	r.Lock.Unlock()
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

// mergeParts rebases and checks every part, then moves every trunk to the
// exact commit that was checked: a commit its worker made meanwhile waits.
func (r *Runner) mergeParts(parts []store.Part) (string, error) {
	trunks, revs := make([]string, len(parts)), make([]string, len(parts))
	for i, p := range parts {
		trunk, rev, err := r.prepareMerge(p)
		if err != nil {
			return "", fmt.Errorf("%s: %w", filepath.Base(p.RepoPath), err)
		}
		trunks[i], revs[i] = trunk, rev
	}
	// The checks took a while: a folder may have got changes meanwhile.
	if why := r.mergeBlocked(parts); why != "" {
		return "", folderBlocked{why: why}
	}
	if err := r.unmovedSinceChecks(parts, revs); err != nil {
		return "", err
	}
	var done []string
	for i, p := range parts {
		name := filepath.Base(p.RepoPath)
		if err := r.MergeGit.FastForward(p.RepoPath, revs[i], trunks[i]); err != nil {
			return "", fmt.Errorf("%s did not merge: %v%s", name, err, mergedSoFar(done))
		}
		done = append(done, name+" into "+trunks[i])
	}
	return "merged " + strings.Join(done, ", "), nil
}

// prepareMerge refuses a part with uncommitted work, rebases it onto its
// trunk, refuses one that changes the repo's gate files, and runs its
// checks, returning the trunk and the commit checked.
func (r *Runner) prepareMerge(p store.Part) (string, string, error) {
	trunk, err := r.Git.Trunk(p.RepoPath)
	if err != nil {
		return "", "", err
	}
	left, err := r.MergeGit.Uncommitted(p.Worktree)
	if err != nil {
		return "", "", err
	}
	if left != "" {
		return "", "", workerFixable{p, fmt.Errorf("uncommitted work in its worktree (%s); commit it or remove it", cut(left, 300))}
	}
	rev, err := r.MergeGit.Rebase(p.Worktree, trunk)
	var clash rebaseClash
	if errors.As(err, &clash) {
		return "", "", workerFixable{p, err}
	}
	if err != nil {
		return "", "", err
	}
	if err := r.gateFilesUntouched(p.RepoPath, trunk, rev); err != nil {
		return "", "", err
	}
	res := r.Checks.Run(p.RepoPath, p.Worktree, trunk)
	switch {
	case !res.Configured:
		return "", "", errors.New("lists no checks (.claude/qa-gates), so nothing shows it is green; merge it by hand")
	case !res.OK:
		return "", "", workerFixable{p, errors.New("checks failed:\n" + res.Output)}
	}
	return trunk, rev, nil
}

// unmovedSinceChecks refuses a feature whose worker folder changed while
// the checks ran in it (a check rewrote files, or its worker or a person
// kept going): the checks then graded files that are not the commit that
// would land.
func (r *Runner) unmovedSinceChecks(parts []store.Part, revs []string) error {
	for i, p := range parts {
		why, err := r.MergeGit.Moved(p.Worktree, revs[i])
		if err != nil {
			return fmt.Errorf("%s: %w", filepath.Base(p.RepoPath), err)
		}
		if why != "" {
			return workerFixable{p, fmt.Errorf("%s: its worktree changed while the checks ran: %s; commit or undo that, then try the merge again", filepath.Base(p.RepoPath), why)}
		}
	}
	return nil
}

// gateFilesUntouched refuses a part that changes anything under .claude/,
// where the repo keeps its gates and their data (a frozen list, say). The
// checks a merge runs are only worth trusting if the worker being graded
// did not write them, so such a part waits for a person to look, however
// green it is. Workers are told to leave .claude/ alone, so this is rare.
func (r *Runner) gateFilesUntouched(repo, trunk, rev string) error {
	changed, err := r.MergeGit.ChangedUnder(repo, trunk, rev, ".claude")
	if err != nil {
		return err
	}
	if len(changed) == 0 {
		return nil
	}
	return fmt.Errorf("it changes the repo's gate files (%s), so it is not merged on its own checks; look them over, then merge it by hand and Mark merged", cut(strings.Join(changed, ", "), 300))
}

// mergedSoFar names the repos already merged when a later one failed: the
// feature is then half in, and a person has to know which half.
func mergedSoFar(done []string) string {
	if len(done) == 0 {
		return ""
	}
	return " (already merged: " + strings.Join(done, ", ") + ")"
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

// tidyPart closes one part's worker and removes its worktree, branch and
// brief, saying what it had to leave ("" when nothing).
func (r *Runner) tidyPart(p store.Part) string {
	if err := r.Host.ClearTaskFolder(p.Worktree); err != nil {
		return fmt.Sprintf("%s (its worker did not close: %v)", p.Worktree, err)
	}
	trunk, err := r.Git.Trunk(p.RepoPath)
	if err != nil {
		return fmt.Sprintf("%s (its trunk unreadable: %v)", p.Worktree, err)
	}
	if err := r.MergeGit.RemoveWorktree(p.RepoPath, p.Worktree, p.Branch, trunk); err != nil {
		return fmt.Sprintf("%s (%v)", p.Worktree, err)
	}
	if err := os.Remove(p.Worktree + ".md"); err != nil && !os.IsNotExist(err) {
		log.Printf("tasks: brief %s.md not removed: %v", p.Worktree, err)
	}
	return ""
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
