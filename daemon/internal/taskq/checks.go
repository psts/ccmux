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
	// Full is the failing line's whole output. The last lines of a test
	// run are often a summary table, not the failure.
	Full string
	// Reran is the lines that failed once and passed when run again.
	Reran []string
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
// and often a person. A line that needed it is named in the result, so a
// test that fails now and then stays seen.
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
		run := g.runLine(worktree, line)
		failed := "failed"
		if run.err != nil && !run.timedOut {
			if run = g.runLine(worktree, line); run.err == nil {
				res.Reran = append(res.Reran, line)
			}
			failed = "failed twice"
		}
		if run.err != nil {
			res.Output = fmt.Sprintf("%s\n(%s: %s: %v)", tail(run.out, 20), failed, line, run.err)
			res.Full = run.out
			return res
		}
	}
	res.OK = true
	return res
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
