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
}

// GateChecks runs the lines of the repo's .claude/qa-gates as the trunk has
// them (the list its human's own gates run), each in its own login shell
// from the worktree's root, the way the qa plugin runs them. A login shell, so the tools the
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
	for _, line := range lines {
		if out, err := g.runLine(worktree, line); err != nil {
			return CheckResult{Configured: true, Output: fmt.Sprintf("%s\n(failed: %s: %v)", tail(out, 20), line, err)}
		}
	}
	return CheckResult{Configured: true, OK: true}
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
