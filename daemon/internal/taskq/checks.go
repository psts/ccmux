package taskq

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

// Checks runs a repo's quality checks in a part's worktree before a merge.
type Checks interface {
	Run(repo, worktree, trunk string) CheckResult
}

// CheckResult is what a check run found. Configured is false when the repo
// lists no checks, so nothing can prove the part is green.
type CheckResult struct {
	Configured bool
	OK         bool
	Output     string // the failing line's output, cut to its last lines
	// Full is the failing line's whole output, of both runs when it was run
	// twice. The last lines of a test run are often a summary table, not
	// the failure.
	Full string
	// Flaked is the lines that failed once and passed when run again. Set
	// on a red result too: a later line may be the one that failed.
	Flaked []Flake
}

// Flake is a gate line that failed once and passed when run again, with
// what its failed run printed: the line alone ("npm test") does not say
// which test it was, and nothing else would.
type Flake struct {
	Line   string
	Output string
}

// GateChecks runs the lines of the repo's .claude/qa-gates as the trunk has
// them (the list its human's own gates run), each in its own login shell
// from the root of the part's worktree, as its worker left it with what it
// installed, the way the qa plugin runs them in a person's own folder. A
// login shell, so the tools the human's profile puts on PATH are there for a
// daemon started without it. A branch that changed the gate files is never
// checked here: the merge stops it first (merge.go).
//
// A line that fails is run once more before it counts. In one batch of
// twenty tasks, twelve merges failed on a check and went back to a worker,
// and not one was the branch's doing: a 250 ms timing test and a suite's
// per-test limit, losing to whatever else the machine was running. A second
// run costs minutes; the worker's loop it saves cost a quarter of an hour
// and often a person. A line that needed it is in the result with what its
// failed run printed, so a test that fails now and then stays seen, by name.
type GateChecks struct {
	PerLine time.Duration // how long one line may run
}

const gatesFile = ".claude/qa-gates"

func (g GateChecks) Run(repo, worktree, trunk string) CheckResult {
	lines, err := gateLines(repo, trunk)
	if err != nil {
		return CheckResult{Configured: !errors.Is(err, os.ErrNotExist), Output: err.Error()}
	}
	if len(lines) == 0 {
		return CheckResult{} // a list of comments checks nothing
	}
	res := CheckResult{Configured: true}
	for _, line := range lines {
		run, first := g.checkLine(worktree, line)
		if run.err != nil {
			res.Output, res.Full = failureOf(line, run, first)
			return res
		}
		if first != nil {
			res.Flaked = append(res.Flaked, Flake{Line: line, Output: first.out})
		}
	}
	res.OK = true
	return res
}

// checkLine runs one gate line, and once more when it failed (not when it
// timed out: it would only take its whole time twice). It returns the run
// that counts and, when there were two, the first as well.
func (g GateChecks) checkLine(dir, line string) (lineRun, *lineRun) {
	run := g.runLine(dir, line)
	if run.err == nil || run.timedOut {
		return run, nil
	}
	return g.runLine(dir, line), &run
}

// failureOf is a failed line's last lines with what happened, and its whole
// output. After two runs both are kept. When the second run only timed out,
// the first is the one that says what is wrong, so its last lines are the
// ones shown, and it is not called two failures.
func failureOf(line string, run lineRun, first *lineRun) (string, string) {
	if first == nil {
		return fmt.Sprintf("%s\n(failed: %s: %v)", tail(run.out, 20), line, run.err), run.out
	}
	full := first.out + "\n--- the same line, run again ---\n" + run.out
	if run.timedOut {
		return fmt.Sprintf("%s\n(failed: %s: %v; run again, it %v)", tail(first.out, 20), line, first.err, run.err), full
	}
	return fmt.Sprintf("%s\n(failed twice: %s: %v)", tail(run.out, 20), line, run.err), full
}

// gateLines reads the checks from the trunk, never from the branch being
// checked: a worker that edited the list on its branch must not grade its
// own work by it. A list the repo does not track is read from its folder;
// a git error is an error, never taken to mean the trunk has no list.
func gateLines(repo, trunk string) ([]string, error) {
	text, err := trunkGates(repo, trunk)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, l := range strings.Split(text, "\n") {
		if l = strings.TrimSpace(l); l != "" && !strings.HasPrefix(l, "#") {
			out = append(out, l)
		}
	}
	return out, nil
}

// trunkGates is the gate list's text: the trunk's copy, or the repo
// folder's own when the trunk does not track one.
func trunkGates(repo, trunk string) (string, error) {
	listed, err := runGit(repo, "ls-tree", "--name-only", trunk, "--", gatesFile)
	if err != nil {
		return "", err
	}
	if listed != "" {
		return runGit(repo, "show", trunk+":"+gatesFile)
	}
	data, err := os.ReadFile(filepath.Join(repo, gatesFile))
	return string(data), err
}

// lineRun is how one run of one line went.
type lineRun struct {
	out      string
	err      error
	timedOut bool // stopped at PerLine: running it again would only take as long
}

// runLine runs one line in its own process group. On time out the whole
// group is killed: killing bash alone leaves what it started (go test, a
// server) holding the output open, and the run would hang past its limit.
func (g GateChecks) runLine(dir, line string) lineRun {
	ctx, cancel := context.WithTimeout(context.Background(), g.PerLine)
	defer cancel()
	cmd := exec.CommandContext(ctx, "bash", "-lc", line)
	cmd.Dir = dir
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
	cmd.WaitDelay = 5 * time.Second
	out, err := cmd.CombinedOutput()
	run := lineRun{out: string(out), err: err}
	if err != nil && ctx.Err() != nil {
		run.timedOut = true
		run.err = fmt.Errorf("timed out after %s: %w", g.PerLine, err)
	}
	return run
}

// tail keeps the last n lines of s.
func tail(s string, n int) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
}
