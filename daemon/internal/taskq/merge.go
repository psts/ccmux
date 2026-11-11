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

// checked is a part ready to land: its trunk, the commit its checks ran
// on, and what to say of checks that passed only on a second run ("" for
// none).
type checked struct {
	trunk, rev string
	flaked     string
}

// mergeParts rebases and checks every part, then moves every trunk to the
// exact commit that was checked: a commit its worker made meanwhile waits.
func (r *Runner) mergeParts(parts []store.Part) (string, error) {
	oks, revs := make([]checked, len(parts)), make([]string, len(parts))
	for i, p := range parts {
		ok, err := r.prepareMerge(p)
		if err != nil {
			return "", fmt.Errorf("%s: %w", filepath.Base(p.RepoPath), err)
		}
		oks[i], revs[i] = ok, ok.rev
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
		if err := r.MergeGit.FastForward(p.RepoPath, oks[i].rev, oks[i].trunk); err != nil {
			return "", fmt.Errorf("%s did not merge: %v%s", name, err, mergedSoFar(done))
		}
		done = append(done, name+" into "+oks[i].trunk)
	}
	return "merged " + strings.Join(done, ", ") + rerunNote(parts, oks), nil
}

// rerunNote says, per repo, which checks failed once and passed when run
// again, for the done note: a test that fails now and then is merged, and
// still seen. "" when every check passed first time.
func rerunNote(parts []store.Part, oks []checked) string {
	var out strings.Builder
	for i, p := range parts {
		if oks[i].flaked != "" {
			fmt.Fprintf(&out, "; %s: %s", filepath.Base(p.RepoPath), oks[i].flaked)
		}
	}
	return out.String()
}

// prepareMerge refuses a part with uncommitted work, rebases it onto its
// trunk, refuses one that changes the repo's gate files, and runs its
// checks, returning what was checked.
func (r *Runner) prepareMerge(p store.Part) (checked, error) {
	trunk, err := r.Git.Trunk(p.RepoPath)
	if err != nil {
		return checked{}, err
	}
	left, err := r.MergeGit.Uncommitted(p.Worktree)
	if err != nil {
		return checked{}, err
	}
	if left != "" {
		return checked{}, workerFixable{p, fmt.Errorf("uncommitted work in its worktree (%s); commit it or remove it", cut(left, 300))}
	}
	rev, err := r.MergeGit.Rebase(p.Worktree, trunk)
	var clash rebaseClash
	if errors.As(err, &clash) {
		return checked{}, workerFixable{p, err}
	}
	if err != nil {
		return checked{}, err
	}
	if err := r.gateFilesUntouched(p.RepoPath, trunk, rev); err != nil {
		return checked{}, err
	}
	flaked, err := r.checkPart(p, trunk)
	if err != nil {
		return checked{}, err
	}
	return checked{trunk: trunk, rev: rev, flaked: flaked}, nil
}

// maxCheckLog is the most of a failed check's output kept in its file: the
// end of it, where a test run lists what failed.
const maxCheckLog = 4 << 20

// checkLog is where a part's failed check leaves its whole output: beside
// the worktree, like the brief, where its worker can read it and no check
// or commit ever sees it.
func checkLog(p store.Part) string { return p.Worktree + ".check.log" }

// flakeLog is where a part's checks that failed once and then passed leave
// what the failed runs printed. It stays after the tidy-up: the feature is
// merged, and this is all that says which test it was.
func flakeLog(p store.Part) string { return p.Worktree + ".flaky.log" }

// checkPart runs the repo's checks in the part's worktree and returns what
// to say of lines that needed a second run ("" for none). A red run goes
// back to its worker with the last lines of the failure and the file
// holding all of it; a green one removes the file an earlier failure left.
func (r *Runner) checkPart(p store.Part, trunk string) (string, error) {
	res := r.Checks.Run(p.RepoPath, p.Worktree, trunk)
	flaked := r.keepFlakes(p, res.Flaked)
	switch {
	case !res.Configured:
		return "", errors.New("lists no checks (.claude/qa-gates), so nothing shows it is green; merge it by hand")
	case !res.OK:
		return "", workerFixable{p, errors.New("checks failed:\n" + res.Output + r.keepCheckOutput(p, res.Full))}
	}
	if err := os.Remove(checkLog(p)); err != nil && !os.IsNotExist(err) {
		log.Printf("tasks: #%d: old check output %s not removed: %v", p.TaskID, checkLog(p), err)
	}
	return flaked, nil
}

// keepFlakes records the checks that failed once and passed when run again:
// a log line each, and what their failed runs printed in the part's flaky
// file. It returns the words for the done note, "" for no flakes. Called
// for a red result too, where a later line failed: the flake is then on
// record, though no done note will carry it.
func (r *Runner) keepFlakes(p store.Part, flakes []Flake) string {
	if len(flakes) == 0 {
		return ""
	}
	var lines []string
	var out strings.Builder
	for _, f := range flakes {
		log.Printf("tasks: #%d part in %s: check %q failed once and passed when run again", p.TaskID, p.RepoPath, f.Line)
		lines = append(lines, f.Line)
		fmt.Fprintf(&out, "=== %s (failed, then passed when run again) ===\n%s\n", f.Line, f.Output)
	}
	said := strings.Join(lines, ", ") + " failed once and passed when run again"
	path := flakeLog(p)
	if err := os.WriteFile(path, []byte(cut(out.String(), maxCheckLog)), 0o644); err != nil {
		log.Printf("tasks: #%d: failed runs' output not kept in %s: %v", p.TaskID, path, err)
		return said
	}
	return said + " (the failed output: " + path + ")"
}

// keepCheckOutput writes a failed check's whole output to the part's file
// and returns the line that names it, "" when there is nothing to keep or
// it could not be written (the failure's last lines still go out). The
// line ends the failure: its first line is what a lens row shows.
func (r *Runner) keepCheckOutput(p store.Part, full string) string {
	if full == "" || p.Worktree == "" {
		return ""
	}
	path := checkLog(p)
	if err := os.WriteFile(path, []byte(cut(full, maxCheckLog)), 0o644); err != nil {
		log.Printf("tasks: #%d: check output not kept in %s: %v", p.TaskID, path, err)
		return ""
	}
	return "\n(the whole output is in " + path + ")"
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

// tidyPart closes one part's worker and removes its worktree, branch, brief
// and any check output, saying what it had to leave ("" when nothing).
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
	for _, f := range []string{p.Worktree + ".md", checkLog(p)} {
		if err := os.Remove(f); err != nil && !os.IsNotExist(err) {
			log.Printf("tasks: %s not removed: %v", f, err)
		}
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
