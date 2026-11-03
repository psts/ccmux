package taskq

import (
	"context"
	"errors"
	"fmt"
	"log"
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
	// TrunkCopies lists the branch's changed files under .claude/ that the
	// checks ran from the trunk's copy instead, so the merge can name them.
	TrunkCopies []string
}

// GateChecks runs the lines of the repo's .claude/qa-gates as the trunk has
// them (the list its human's own gates run), each in its own login shell
// from the root of a throwaway copy of the part (checkBox), the way the qa
// plugin runs them. A login shell, so the tools the
// human's profile puts on PATH are there for a daemon started without it.
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
	box, changed, drop, err := checkBox(repo, worktree, trunk)
	if err != nil {
		return CheckResult{Configured: true, Output: "could not set up the checks: " + err.Error()}
	}
	defer drop()
	for _, line := range lines {
		if out, err := g.runLine(box, line); err != nil {
			return CheckResult{Configured: true, Output: fmt.Sprintf("%s\n(failed: %s: %v)", tail(out, 20), line, err), TrunkCopies: changed}
		}
	}
	return CheckResult{Configured: true, OK: true, TrunkCopies: changed}
}

// checkBox makes the place the checks run: a throwaway worktree of the
// part's commit with the trunk's .claude/ in it, so the gate scripts and
// their data (a frozen list, say) grading the branch are the trunk's, not
// the worker's. The worker's own worktree is never touched, so nothing
// has to be put back, and a daemon that dies mid-check leaves only the box,
// cleared the next time. Only committed work is in it, which is all the
// merge takes. changed lists the branch's .claude/ files the trunk's
// copies stood in for.
func checkBox(repo, worktree, trunk string) (string, []string, func(), error) {
	rev, err := runGit(worktree, "rev-parse", "HEAD")
	if err != nil {
		return "", nil, nil, err
	}
	box := worktree + ".checks"
	dropBox(repo, box) // one a restart left behind
	if _, err := runGit(repo, "worktree", "add", "--detach", box, rev); err != nil {
		return "", nil, nil, err
	}
	drop := func() { dropBox(repo, box) }
	changed, err := changedGateFiles(repo, trunk, rev)
	if err == nil && len(changed) > 0 {
		_, err = runGit(box, "restore", "--source="+trunk, "--staged", "--worktree", "--", ".claude")
	}
	if err != nil {
		drop()
		return "", nil, nil, err
	}
	return box, changed, drop, nil
}

// changedGateFiles lists the files under .claude/ that differ between the
// trunk and rev: renames as two paths, names unquoted.
func changedGateFiles(repo, trunk, rev string) ([]string, error) {
	out, err := runGit(repo, "diff", "--name-only", "--no-renames", "-z", trunk, rev, "--", ".claude")
	if err != nil {
		return nil, err
	}
	var paths []string
	for _, p := range strings.Split(out, "\x00") {
		if p != "" {
			paths = append(paths, p)
		}
	}
	return paths, nil
}

// dropBox removes a check box and its worktree record. The folder is the
// runner's own, so if git will not remove it, it is deleted outright.
func dropBox(repo, box string) {
	if _, err := os.Stat(box); err == nil {
		if _, err := runGit(repo, "worktree", "remove", "--force", box); err != nil {
			log.Printf("tasks: check box %s: %v; deleting it", box, err)
			_ = os.RemoveAll(box)
		}
	}
	_, _ = runGit(repo, "worktree", "prune")
}

// gateLines reads the checks from the trunk, never from the branch being
// checked: a worker that edited the list on its branch must not grade its
// own work by it. A list the repo does not track is read from its folder.
func gateLines(repo, trunk string) ([]string, error) {
	text, err := runGit(repo, "show", trunk+":"+gatesFile)
	if err != nil {
		data, ferr := os.ReadFile(filepath.Join(repo, gatesFile))
		if ferr != nil {
			return nil, ferr
		}
		text = string(data)
	}
	var out []string
	for _, l := range strings.Split(text, "\n") {
		if l = strings.TrimSpace(l); l != "" && !strings.HasPrefix(l, "#") {
			out = append(out, l)
		}
	}
	return out, nil
}

// runLine runs one line in its own process group. On time out the whole
// group is killed: killing bash alone leaves what it started (go test, a
// server) holding the output open, and the run would hang past its limit.
func (g GateChecks) runLine(dir, line string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), g.PerLine)
	defer cancel()
	cmd := exec.CommandContext(ctx, "bash", "-lc", line)
	cmd.Dir = dir
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
	cmd.WaitDelay = 5 * time.Second
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// tail keeps the last n lines of s.
func tail(s string, n int) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
}
