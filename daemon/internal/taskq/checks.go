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
	swapped, restore, err := useTrunkGates(worktree, trunk)
	if err != nil {
		return CheckResult{Configured: true, Output: "could not put the trunk's .claude/ in place for the checks: " + err.Error()}
	}
	defer restore()
	for _, line := range lines {
		if out, err := g.runLine(worktree, line); err != nil {
			return CheckResult{Configured: true, Output: fmt.Sprintf("%s\n(failed: %s: %v)", tail(out, 20), line, err), TrunkCopies: swapped}
		}
	}
	return CheckResult{Configured: true, OK: true, TrunkCopies: swapped}
}

// useTrunkGates puts the trunk's version of every file under .claude/ the
// branch changed into the worktree for the checks (removing one the trunk
// lacks), so the gate scripts and their data (a frozen list, say) grading
// the branch are the trunk's, not the worker's. Only working files change;
// restore puts the branch's back from its index, which the merge checked
// is clean, so the merge itself still takes the branch's .claude/ changes.
func useTrunkGates(worktree, trunk string) ([]string, func(), error) {
	out, err := runGit(worktree, "diff", "--name-only", trunk, "HEAD", "--", ".claude")
	if err != nil || out == "" {
		return nil, func() {}, err
	}
	paths := strings.Split(out, "\n")
	for _, p := range paths {
		if err := placeTrunkCopy(worktree, trunk, p); err != nil {
			restoreBranchCopies(worktree, paths)
			return nil, func() {}, err
		}
	}
	return paths, func() { restoreBranchCopies(worktree, paths) }, nil
}

func placeTrunkCopy(worktree, trunk, path string) error {
	full := filepath.Join(worktree, path)
	text, err := runGit(worktree, "show", trunk+":"+path)
	if err != nil { // not on the trunk: the branch added it
		if rmErr := os.Remove(full); rmErr != nil && !os.IsNotExist(rmErr) {
			return rmErr
		}
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		return err
	}
	return os.WriteFile(full, []byte(text+"\n"), 0o755)
}

// restoreBranchCopies puts the branch's files back as committed, and drops
// a file the branch does not have.
func restoreBranchCopies(worktree string, paths []string) {
	for _, p := range paths {
		if _, err := runGit(worktree, "cat-file", "-e", "HEAD:"+p); err == nil {
			if _, err := runGit(worktree, "checkout", "--", p); err != nil {
				log.Printf("tasks: %s: %s not restored after the checks: %v", worktree, p, err)
			}
			continue
		}
		_ = os.Remove(filepath.Join(worktree, p))
	}
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
