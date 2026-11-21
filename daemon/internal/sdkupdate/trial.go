package sdkupdate

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"time"

	"ccmux.dev/ccmuxd/internal/runner"
	"ccmux.dev/ccmuxd/internal/store"
	"github.com/google/uuid"
)

// A trial is a real session on a candidate SDK: the runner from the
// candidate's folder, started with the same flags a person's session gets
// (runner.Config.BuildSpec), in an empty folder, on Haiku, with no pane and
// no bus. It passes when one prompt gets a finished turn with an answer.
// It costs one short Haiku turn per Claude it is run with.

const (
	trialUp     = time.Minute
	trialTurn   = 3 * time.Minute
	trialPrompt = "This is an automatic check of a software update. Reply with the single word OK."
)

// TrialSession is Updater.Trial for this host: node and the runner
// environment come from the caller. tag names this daemon's trial folders
// (TrialTag), so CleanTrials never touches another daemon's.
func TrialSession(node string, env func() []string, tag string) func(ctx context.Context, script, claude string) error {
	return func(ctx context.Context, script, claude string) error {
		return trial(ctx, node, env, tag, script, claude)
	}
}

// TrialTag is the name every trial folder of the daemon whose agents live
// under agentsRoot starts with: short, since a unix socket path inside it
// is capped near 108 bytes.
func TrialTag(agentsRoot string) string {
	sum := sha256.Sum256([]byte(agentsRoot))
	return "ccsdk-" + hex.EncodeToString(sum[:3]) + "-"
}

func trial(ctx context.Context, node string, env func() []string, tag, script, claude string) error {
	root, err := os.MkdirTemp("", tag)
	if err != nil {
		return err
	}
	work := filepath.Join(root, "work")
	defer cleanTrial(root, work)
	if err := os.MkdirAll(work, 0o700); err != nil {
		return err
	}
	sess := store.RunnerSession{ID: uuid.NewString(), Cwd: work, Model: "haiku", Name: "sdk-trial"}
	paths := runner.Paths{State: filepath.Join(root, "st"), Runtime: filepath.Join(root, "s"), Prefix: "ccmux-sdk-trial"}
	cfg := runner.Config{Sidecar: func() (string, string, error) { return script, node, nil }, Claude: claude, Env: env}
	sp, err := cfg.BuildSpec(sess, paths)
	if err != nil {
		return err
	}
	for _, d := range []string{paths.State, paths.Runtime, paths.MediaDir(sess.ID)} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			return err
		}
	}
	cmd := exec.Command(sp.Node, append([]string{sp.Script, "serve"}, sp.Args...)...)
	cmd.Dir, cmd.Env = sp.Dir, sp.Env
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	var out strings.Builder
	cmd.Stdout, cmd.Stderr = &out, &out
	if err := cmd.Start(); err != nil {
		return err
	}
	defer stopTrial(cmd)
	// The daemon runs with KillMode=process: a restart in the middle of a
	// trial leaves this runner up. Its pid here lets the next start stop it.
	if err := os.WriteFile(filepath.Join(root, "pid"), []byte(strconv.Itoa(cmd.Process.Pid)), 0o600); err != nil {
		return err
	}
	if err := runTurn(ctx, runner.Dial(paths.OpenSocket(sess.ID))); err != nil {
		return fmt.Errorf("%w; runner said: %s", err, lastLines(out.String(), 5))
	}
	return nil
}

// runTurn waits for the runner, sends the prompt, and waits for an answer.
func runTurn(ctx context.Context, c *runner.Client) error {
	if err := waitUp(ctx, c); err != nil {
		return err
	}
	ask, cancel := context.WithTimeout(ctx, 10*time.Second)
	sent, err := c.Prompt(ask, "sdk-trial", trialPrompt, nil)
	cancel()
	if err != nil || !sent {
		return fmt.Errorf("the prompt was not taken: %v", err)
	}
	deadline := time.Now().Add(trialTurn)
	for time.Now().Before(deadline) {
		if done, err := turnDone(ctx, c); done || err != nil {
			return err
		}
		if err := pause(ctx, 2*time.Second); err != nil {
			return err
		}
	}
	return fmt.Errorf("no answer within %s", trialTurn)
}

func waitUp(ctx context.Context, c *runner.Client) error {
	deadline := time.Now().Add(trialUp)
	for time.Now().Before(deadline) {
		ask, cancel := context.WithTimeout(ctx, 5*time.Second)
		_, err := c.Hello(ask)
		cancel()
		if err == nil {
			return nil
		}
		if err := pause(ctx, time.Second); err != nil {
			return err
		}
	}
	return fmt.Errorf("the runner did not answer within %s", trialUp)
}

func pause(ctx context.Context, d time.Duration) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(d):
		return nil
	}
}

// turnDone reads the snapshot: done once a turn ended with Claude's text,
// an error when it ended in one.
func turnDone(ctx context.Context, c *runner.Client) (bool, error) {
	ask, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	snap, err := c.Snapshot(ask)
	if err != nil {
		return false, nil // asked again at the next look
	}
	return answered(snap.Body)
}

// answered says whether a snapshot holds a finished turn with Claude's
// words, or the error a turn ended in.
func answered(body json.RawMessage) (bool, error) {
	var snap struct {
		Busy     bool `json:"busy"`
		Messages []struct {
			Info struct {
				Role  string          `json:"role"`
				Error json.RawMessage `json:"error"`
			} `json:"info"`
			Parts []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"parts"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(body, &snap); err != nil {
		return false, err
	}
	if snap.Busy {
		return false, nil
	}
	for _, m := range snap.Messages {
		if m.Info.Role != "assistant" {
			continue
		}
		if len(m.Info.Error) > 0 && string(m.Info.Error) != "null" {
			return true, errors.New("the turn ended in an error: " + string(m.Info.Error))
		}
		for _, p := range m.Parts {
			if p.Type == "text" && strings.TrimSpace(p.Text) != "" {
				return true, nil
			}
		}
	}
	return false, nil
}

// stopTrial ends the runner and everything it started.
func stopTrial(cmd *exec.Cmd) {
	if cmd.Process == nil {
		return
	}
	_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM)
	done := make(chan struct{})
	go func() { _ = cmd.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		<-done
	}
}

// CleanTrials stops and removes what trials of this daemon (tag) left when
// it was stopped in the middle of one: the runner, if the pid it wrote is
// still that runner (its command line names the trial's folder), the
// folder, and its conversation. Run once at start, before any trial.
func CleanTrials(tag string) {
	roots, _ := filepath.Glob(filepath.Join(os.TempDir(), tag+"*"))
	for _, root := range roots {
		if pid := trialPid(root); pid > 0 {
			_ = syscall.Kill(-pid, syscall.SIGKILL) // its own process group (Setpgid)
		}
		cleanTrial(root, filepath.Join(root, "work"))
	}
}

// trialPid is the runner a trial folder names, 0 unless that process is
// still running with the folder on its command line: a pid reused since
// is someone else's.
func trialPid(root string) int {
	body, err := os.ReadFile(filepath.Join(root, "pid"))
	if err != nil {
		return 0
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(body)))
	if err != nil || pid <= 1 {
		return 0
	}
	cmdline, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "cmdline"))
	if err != nil || !strings.Contains(string(cmdline), root+"/") {
		return 0
	}
	return pid
}

var projectName = regexp.MustCompile(`[^A-Za-z0-9]`)

// cleanTrial removes the trial's folder and the conversation Claude Code
// kept for it under ~/.claude/projects (named after the folder).
func cleanTrial(root, work string) {
	_ = os.RemoveAll(root)
	home, err := os.UserHomeDir()
	if err != nil {
		return
	}
	_ = os.RemoveAll(filepath.Join(home, ".claude", "projects", projectName.ReplaceAllString(work, "-")))
}

func lastLines(s string, n int) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, " | ")
}
