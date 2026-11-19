package taskq

import (
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
)

// Landing is the merge itself: a set of branches, each in its own worktree
// of its own repo, lands as one. Every branch is rebased onto its repo's
// trunk and checked there, and only when all of them are green does each
// trunk move forward, a fast-forward only. Nothing is pushed.
//
// Two callers land work this way: the task queue, a feature's parts
// (merge.go), and a thread with helpers, its members' branches
// (internal/threads). What differs between them (who is told of a failure,
// who closes the worker) is theirs; the git and the checks are here.

// Work is one branch to land: its repo, the worktree the branch is checked
// out in, and the branch. Label says whose it is in the log ("#12").
type Work struct {
	RepoPath, Worktree, Branch string
	Label                      string
}

// Lander lands work. Tag opens its log lines ("tasks", "threads").
type Lander struct {
	Git      Git
	MergeGit MergeGit
	Checks   Checks
	Tag      string
	// Steps and Halt are for a landing that runs as a job (landjob.go),
	// which can be cut off and run again. Both nil, a landing runs as it
	// always did.
	//
	// Steps remembers the steps a landing finished: a second run skips the
	// checks of a commit they already passed on, and says what it is doing
	// meanwhile.
	Steps Steps
	// Halt is asked before each piece of work is prepared and once more
	// before the first trunk moves; an error stops the landing there. It is
	// never asked between two trunks: a landing that started moving them
	// moves them all.
	Halt func() error
}

// Steps is a record of finished steps that outlives the run that made it
// (a job's journal). A key names exactly what was done, so work that
// changed since has another key.
type Steps interface {
	Done(key string, into any) bool
	Begin(key, label string)
	Finished(key string, data any)
}

func (l Lander) begin(key, label string) {
	if l.Steps != nil {
		l.Steps.Begin(key, label)
	}
}

func (l Lander) finished(key string, data any) {
	if l.Steps != nil {
		l.Steps.Finished(key, data)
	}
}

func (l Lander) halted() error {
	if l.Halt == nil {
		return nil
	}
	return l.Halt()
}

// landedKey names the step that moved a repo's trunk to a commit.
const landedKey = "landed:"

// fixable is a failure inside one piece of work's own worktree (its branch
// clashes with the trunk, the checks failed on it, it left work uncommitted,
// it changed while the checks ran): whoever works there can fix it. index
// is its place in the list that was landed.
type fixable struct {
	index int
	err   error
}

func (e fixable) Error() string { return e.err.Error() }
func (e fixable) Unwrap() error { return e.err }

// Fixable reports whether a failed landing is one inside a piece of work's
// own worktree, and which one (its place in the list that was landed).
func Fixable(err error) (int, bool) {
	var fix fixable
	if errors.As(err, &fix) {
		return fix.index, true
	}
	return 0, false
}

// Blocked says why the work cannot land now, "" when it can: a repo's own
// folder holds uncommitted changes on the trunk, has the trunk checked out
// elsewhere, or git is busy there.
func (l Lander) Blocked(work []Work) string {
	for _, w := range work {
		trunk, err := l.Git.Trunk(w.RepoPath)
		if err != nil {
			return "waiting: " + err.Error()
		}
		why, err := l.MergeGit.TrunkBlocked(w.RepoPath, trunk)
		if err != nil {
			return "waiting: " + err.Error()
		}
		if why != "" {
			return "waiting to merge: " + why
		}
	}
	return ""
}

// checked is a piece of work ready to land: its trunk, the commit its
// checks ran on, and what to say of checks that passed only on a second run
// ("" for none).
type checked struct {
	trunk, rev string
	flaked     string
}

// Land rebases and checks every piece of work, then moves every trunk to
// the exact commit that was checked: a commit made meanwhile waits. It
// returns what to say of a landing that worked.
func (l Lander) Land(work []Work) (string, error) {
	oks, revs := make([]checked, len(work)), make([]string, len(work))
	for i, w := range work {
		if err := l.halted(); err != nil {
			return "", err
		}
		ok, err := l.prepare(i, w)
		if err != nil {
			return "", fmt.Errorf("%s: %w", filepath.Base(w.RepoPath), err)
		}
		oks[i], revs[i] = ok, ok.rev
	}
	// The checks took a while: a folder may have got changes meanwhile.
	if why := l.Blocked(work); why != "" {
		return "", folderBlocked{why: why}
	}
	if err := l.unmovedSinceChecks(work, revs); err != nil {
		return "", err
	}
	if err := l.halted(); err != nil {
		return "", err
	}
	done, err := l.forward(work, oks)
	if err != nil {
		return "", err
	}
	return "merged " + strings.Join(done, ", ") + rerunNote(work, oks), nil
}

// forward moves each repo's trunk to its checked commit and returns what
// landed. Moving a trunk that is there already changes nothing, so a
// landing run again after it was cut off here goes through the repos it
// had done and carries on with the rest.
func (l Lander) forward(work []Work, oks []checked) ([]string, error) {
	var done []string
	for i, w := range work {
		name := filepath.Base(w.RepoPath)
		l.begin("landing:"+w.RepoPath, name+": moving "+oks[i].trunk+" forward")
		if err := l.MergeGit.FastForward(w.RepoPath, oks[i].rev, oks[i].trunk); err != nil {
			return nil, fmt.Errorf("%s did not merge: %v%s", name, err, mergedSoFar(done))
		}
		done = append(done, name+" into "+oks[i].trunk)
		l.finished(landedKey+w.RepoPath+"@"+oks[i].rev, name+" into "+oks[i].trunk)
	}
	return done, nil
}

// rerunNote says, per repo, which checks failed once and passed when run
// again, for the done note: a test that fails now and then is merged, and
// still seen. "" when every check passed first time.
func rerunNote(work []Work, oks []checked) string {
	var out strings.Builder
	for i, w := range work {
		if oks[i].flaked != "" {
			fmt.Fprintf(&out, "; %s: %s", filepath.Base(w.RepoPath), oks[i].flaked)
		}
	}
	return out.String()
}

// prepare refuses work with uncommitted files, rebases it onto its trunk,
// refuses one that changes the repo's gate files, and runs its checks,
// returning what was checked. i is the work's place in the list, for a
// failure its own worker can fix.
func (l Lander) prepare(i int, w Work) (checked, error) {
	trunk, err := l.Git.Trunk(w.RepoPath)
	if err != nil {
		return checked{}, err
	}
	left, err := l.MergeGit.Uncommitted(w.Worktree)
	if err != nil {
		return checked{}, err
	}
	if left != "" {
		return checked{}, fixable{i, fmt.Errorf("uncommitted work in its worktree (%s); commit it or remove it%s", cut(left, 300), linkHint(w.Worktree, left))}
	}
	l.begin("rebase:"+w.Worktree, filepath.Base(w.RepoPath)+": rebasing onto "+trunk)
	rev, err := l.MergeGit.Rebase(w.Worktree, trunk)
	var clash rebaseClash
	if errors.As(err, &clash) {
		return checked{}, fixable{i, err}
	}
	if err != nil {
		return checked{}, err
	}
	if err := l.gateFilesUntouched(w.RepoPath, trunk, rev); err != nil {
		return checked{}, err
	}
	flaked, err := l.checkOnce(i, w, trunk, rev)
	if err != nil {
		return checked{}, err
	}
	return checked{trunk: trunk, rev: rev, flaked: flaked}, nil
}

// checkOnce runs the work's checks unless an earlier run of this landing
// already saw them pass on this very commit: the checks are the minutes of
// a landing, and a second run on the same files proves nothing new. A
// commit that changed since (the trunk moved, so the rebase gave another)
// has another key and is checked.
func (l Lander) checkOnce(i int, w Work, trunk, rev string) (string, error) {
	key := "checked:" + w.Worktree + "@" + rev
	var flaked string
	if l.Steps != nil && l.Steps.Done(key, &flaked) {
		log.Printf("%s: %s part in %s: its checks passed on %s in an earlier run; not run again", l.Tag, w.Label, w.RepoPath, rev)
		return flaked, nil
	}
	l.begin(key, filepath.Base(w.RepoPath)+": checks running")
	flaked, err := l.check(i, w, trunk)
	if err == nil {
		l.finished(key, flaked)
	}
	return flaked, err
}

// maxCheckLog is the most of a failed check's output kept in its file: the
// end of it, where a test run lists what failed.
const maxCheckLog = 4 << 20

// checkLog is where a failed check leaves its whole output: beside the
// worktree, where its worker can read it and no check or commit ever sees
// it.
func checkLog(worktree string) string { return worktree + ".check.log" }

// flakeLog is where checks that failed once and then passed leave what the
// failed runs printed. It stays after the tidy-up: the work is merged, and
// this is all that says which test it was.
func flakeLog(worktree string) string { return worktree + ".flaky.log" }

// check runs the repo's checks in the work's worktree and returns what to
// say of lines that needed a second run ("" for none). A red run goes back
// to its worker with the last lines of the failure and the file holding all
// of it; a green one removes the file an earlier failure left.
func (l Lander) check(i int, w Work, trunk string) (string, error) {
	res := l.Checks.Run(w.RepoPath, w.Worktree, trunk)
	flaked := l.keepFlakes(w, res.Flaked)
	switch {
	case !res.Configured:
		return "", errors.New("lists no checks (.claude/qa-gates), so nothing shows it is green; merge it by hand")
	case !res.OK:
		return "", fixable{i, errors.New("checks failed:\n" + res.Output + l.keepCheckOutput(w, res.Full))}
	}
	if err := os.Remove(checkLog(w.Worktree)); err != nil && !os.IsNotExist(err) {
		log.Printf("%s: %s: old check output %s not removed: %v", l.Tag, w.Label, checkLog(w.Worktree), err)
	}
	return flaked, nil
}

// keepFlakes records the checks that failed once and passed when run again:
// a log line each, and what their failed runs printed in the work's flaky
// file. It returns the words for the done note, "" for no flakes. Called
// for a red result too, where a later line failed: the flake is then on
// record, though no done note will carry it.
func (l Lander) keepFlakes(w Work, flakes []Flake) string {
	if len(flakes) == 0 {
		return ""
	}
	var lines []string
	var out strings.Builder
	for _, f := range flakes {
		log.Printf("%s: %s part in %s: check %q failed once and passed when run again", l.Tag, w.Label, w.RepoPath, f.Line)
		lines = append(lines, f.Line)
		fmt.Fprintf(&out, "=== %s (failed, then passed when run again) ===\n%s\n", f.Line, f.Output)
	}
	said := strings.Join(lines, ", ") + " failed once and passed when run again"
	path := flakeLog(w.Worktree)
	if err := os.WriteFile(path, []byte(cut(out.String(), maxCheckLog)), 0o644); err != nil {
		log.Printf("%s: %s: failed runs' output not kept in %s: %v", l.Tag, w.Label, path, err)
		return said
	}
	return said + " (the failed output: " + path + ")"
}

// keepCheckOutput writes a failed check's whole output to the work's file
// and returns the line that names it, "" when there is nothing to keep or
// it could not be written (the failure's last lines still go out). The
// line ends the failure: its first line is what a lens row shows.
func (l Lander) keepCheckOutput(w Work, full string) string {
	if full == "" || w.Worktree == "" {
		return ""
	}
	path := checkLog(w.Worktree)
	if err := os.WriteFile(path, []byte(cut(full, maxCheckLog)), 0o644); err != nil {
		log.Printf("%s: %s: check output not kept in %s: %v", l.Tag, w.Label, path, err)
		return ""
	}
	return "\n(the whole output is in " + path + ")"
}

// unmovedSinceChecks refuses work whose worktree changed while the checks
// ran in it (a check rewrote files, or its worker or a person kept going):
// the checks then graded files that are not the commit that would land.
func (l Lander) unmovedSinceChecks(work []Work, revs []string) error {
	for i, w := range work {
		why, err := l.MergeGit.Moved(w.Worktree, revs[i])
		if err != nil {
			return fmt.Errorf("%s: %w", filepath.Base(w.RepoPath), err)
		}
		if why != "" {
			return fixable{i, fmt.Errorf("%s: its worktree changed while the checks ran: %s; commit or undo that, then try the merge again", filepath.Base(w.RepoPath), why)}
		}
	}
	return nil
}

// gateFilesUntouched refuses work that changes anything under .claude/,
// where the repo keeps its gates and their data (a frozen list, say). The
// checks a merge runs are only worth trusting if the worker being graded
// did not write them, so such work waits for a person to look, however
// green it is. Workers are told to leave .claude/ alone, so this is rare.
func (l Lander) gateFilesUntouched(repo, trunk, rev string) error {
	changed, err := l.MergeGit.ChangedUnder(repo, trunk, rev, ".claude")
	if err != nil {
		return err
	}
	if len(changed) == 0 {
		return nil
	}
	return fmt.Errorf("it changes the repo's gate files (%s), so it is not merged on its own checks; look them over, then merge it by hand and Mark merged", cut(strings.Join(changed, ", "), 300))
}

// mergedSoFar names the repos already merged when a later one failed: the
// work is then half in, and a person has to know which half.
func mergedSoFar(done []string) string {
	if len(done) == 0 {
		return ""
	}
	return " (already merged: " + strings.Join(done, ", ") + ")"
}

// Remove removes landed work's worktree and branch, its check output, and
// the files named beside it (a task's brief), saying what it had to leave
// ("" when nothing). Nothing is forced: a worktree that holds anything
// uncommitted stays, and so does a branch with a commit the trunk lacks.
// Whoever worked there must be closed first; that is the caller's.
func (l Lander) Remove(w Work, beside ...string) string {
	trunk, err := l.Git.Trunk(w.RepoPath)
	if err != nil {
		return fmt.Sprintf("%s (its trunk unreadable: %v)", w.Worktree, err)
	}
	if err := l.MergeGit.RemoveWorktree(w.RepoPath, w.Worktree, w.Branch, trunk); err != nil {
		return fmt.Sprintf("%s (%v)", w.Worktree, err)
	}
	for _, f := range append(beside, checkLog(w.Worktree)) {
		if err := os.Remove(f); err != nil && !os.IsNotExist(err) {
			log.Printf("%s: %s not removed: %v", l.Tag, f, err)
		}
	}
	return ""
}
