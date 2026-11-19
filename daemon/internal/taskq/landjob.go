package taskq

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"strings"
	"time"

	"ccmux.dev/ccmuxd/internal/jobs"
)

// A landing as a job (internal/jobs): the same Land, run by `ccmuxd
// job-exec` in its own service instead of inside the daemon, so a daemon
// that restarts mid-merge neither cuts it off nor leaves its checks running
// with nobody waiting for them. The job holds each repo's lock for the
// whole of it, journals what it finished, and writes how it went as its
// result; whoever started it (a thread's merge) reads that in the daemon.

// LandInput is what a landing job is started from.
type LandInput struct {
	Work []Work `json:"work"`
	// Tag opens the job's log lines, as Lander.Tag does.
	Tag string `json:"tag"`
	// PerLineSeconds is how long one line of a repo's checks may run.
	PerLineSeconds int `json:"perLineSeconds"`
}

// LandOutput is how a landing went. OK with Note (what Land returns for a
// landing that worked), or Error with what kind of failure it was: Fixable
// is the place in Work of the piece whose own worker can fix it (-1 for
// none, what Fixable reads off an error in one process), Blocked a repo's
// own folder that got in the way after the checks.
type LandOutput struct {
	OK      bool   `json:"ok"`
	Note    string `json:"note,omitempty"`
	Error   string `json:"error,omitempty"`
	Fixable int    `json:"fixable"`
	Blocked bool   `json:"blocked,omitempty"`
	// Landed is every trunk this job moved, in all of its runs ("app into
	// main"): on a failure, the half that is already in.
	Landed []string `json:"landed,omitempty"`
	// CheckLog is the file that holds a failed check's whole output, "" when
	// the failure was not a check or left none.
	CheckLog string `json:"checkLog,omitempty"`
}

// Err is the landing's failure as Land would have returned it, of the same
// kind (Fixable still finds its piece of work), nil for one that worked.
func (o LandOutput) Err() error {
	switch {
	case o.OK:
		return nil
	case o.Blocked:
		return folderBlocked{why: o.Error}
	case o.Fixable >= 0:
		return fixable{index: o.Fixable, err: errors.New(o.Error)}
	}
	return errors.New(o.Error)
}

// RunLand is the landing job's Runner. It takes the lock of every repo it
// lands in before it touches one, so no other landing job works in them
// meanwhile, and keeps it until the landing is over (or its process dies).
func RunLand(r *jobs.Run) (any, error) {
	var in LandInput
	if err := json.Unmarshal(r.Spec.Input, &in); err != nil {
		return nil, fmt.Errorf("the landing's input is not readable: %w", err)
	}
	repos := make([]string, len(in.Work))
	for i, w := range in.Work {
		repos[i] = w.RepoPath
	}
	if err := r.LockRepos(repos); err != nil {
		return nil, err
	}
	defer r.Unlock()
	git := markedGit{}
	git.undoCutRebases(in.Tag, in.Work)
	l := Lander{
		Git: git, MergeGit: git, Tag: in.Tag, Steps: runSteps{r}, Halt: r.Ctx.Err,
		Checks: GateChecks{PerLine: time.Duration(in.PerLineSeconds) * time.Second, Ctx: r.Ctx},
	}
	note, err := l.Land(in.Work)
	if err != nil && r.Ctx.Err() != nil {
		return nil, r.Ctx.Err() // cut off by the job's time limit, whatever it looked like from inside
	}
	return LandResult(in.Work, note, err, landedSoFar(r.Journal)), nil
}

// LandResult is a landing's end (what Land returned for work) as a job's
// result carries it, with the trunks the job is known to have moved.
func LandResult(work []Work, note string, err error, landed []string) LandOutput {
	out := LandOutput{OK: err == nil, Note: note, Fixable: -1, Landed: landed}
	if err == nil {
		return out
	}
	out.Error = err.Error()
	var blocked folderBlocked
	out.Blocked = errors.As(err, &blocked)
	if i, ok := Fixable(err); ok && i < len(work) {
		out.Fixable = i
		if path := checkLog(work[i].Worktree); strings.Contains(out.Error, path) {
			out.CheckLog = path
		}
	}
	return out
}

// landedSoFar reads off a landing job's journal which trunks it moved.
func landedSoFar(j *jobs.Journal) []string {
	var out []string
	for _, raw := range j.Recorded(landedKey) {
		var said string
		if json.Unmarshal(raw, &said) == nil && said != "" {
			out = append(out, said)
		}
	}
	return out
}

// Landed reads the same off the entries of a landing job's journal, for
// whoever settles a job that left no result.
func Landed(entries []jobs.Entry) []string {
	var out []string
	for _, e := range entries {
		var said string
		if strings.HasPrefix(e.Key, landedKey) && json.Unmarshal(e.Data, &said) == nil && said != "" {
			out = append(out, said)
		}
	}
	return out
}

// runSteps is a job's journal as a landing's Steps.
type runSteps struct{ r *jobs.Run }

func (s runSteps) Done(key string, into any) bool { return s.r.Journal.Done(key, into) }
func (s runSteps) Begin(key, label string)        { s.r.Step(key, label) }
func (s runSteps) Finished(key string, data any)  { s.r.Finished(key, data) }

// markedGit is ExecGit for a landing job. It differs in one thing: a job
// can be killed in the middle of a rebase, which leaves the worktree
// mid-rebase, and git then refuses every rebase after it. So a rebase
// leaves a mark beside the worktree while it runs, and a job that finds the
// mark at its start undoes the rebase that left it. Only a rebase a
// landing job itself was cut off in is undone this way: a worktree its own
// worker left mid-rebase has no mark, and fails as it always did.
type markedGit struct{ ExecGit }

// RebaseMark is the file a landing job leaves beside a worktree while it
// rebases there.
func RebaseMark(worktree string) string { return worktree + ".rebasing" }

func (g markedGit) Rebase(worktree, trunk string) (string, error) {
	mark := RebaseMark(worktree)
	if err := os.WriteFile(mark, []byte(time.Now().Format(time.RFC3339)+"\n"), 0o644); err != nil {
		return "", fmt.Errorf("marking the rebase in %s: %w", worktree, err)
	}
	rev, err := g.ExecGit.Rebase(worktree, trunk)
	var clash rebaseClash
	if err == nil || errors.As(err, &clash) {
		os.Remove(mark) // done, or undone: the worktree is not mid-rebase
	}
	return rev, err
}

// undoCutRebases undoes the rebase an earlier landing job was killed in,
// in every worktree that carries the mark of one.
func (g markedGit) undoCutRebases(tag string, work []Work) {
	for _, w := range work {
		mark := RebaseMark(w.Worktree)
		if _, err := os.Stat(mark); err != nil {
			continue
		}
		_, err := runGit(w.Worktree, "rebase", "--abort")
		log.Printf("%s: %s: an earlier merge was cut off while rebasing %s; that rebase is undone (%v)", tag, w.Label, w.Worktree, err)
		os.Remove(mark)
	}
}
