// Package runner keeps Claude sessions running outside the daemon: each one
// is a session runner (the claude sidecar in internal/agent/claudeserve,
// started with --socket) in its own systemd user service, so a daemon
// restart touches none of them, a runner that dies is restarted by systemd
// and resumes its conversation, and after a reboot the daemon starts every
// session that should run. The daemon's records (store/sessions.go) say what
// should run; systemd says what does; the transcript says what happened.
package runner

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// templateText is the unit template. Each instance reads its spec file
// (spec.go) through the daemon binary's runner-exec verb, which becomes the
// runner. Restart=on-failure brings back a runner that crashed; nothing
// enables the instances, because the daemon decides at its start which
// sessions should run. KillMode=control-group stops a session's commands
// with it. StandardInput is /dev/null: a runner has no terminal. The start
// limit is the restart cap (startLimitBurst).
func templateText(execPath, specDir string) string {
	return fmt.Sprintf(`[Unit]
Description=ccmux session %%i
StartLimitIntervalSec=%d
StartLimitBurst=%d

[Service]
Type=simple
ExecStart=%s runner-exec %s/%%i.json
Restart=on-failure
RestartSec=2
KillMode=control-group
TimeoutStopSec=20
StandardInput=null
`, startLimitWindow, startLimitBurst, execPath, specDir)
}

// startLimitBurst starts in startLimitWindow seconds, crashes and the
// daemon's own starts alike, and systemd refuses more: the unit fails with
// result start-limit-hit and the supervisor puts the session to sleep, so
// a session that cannot stay up stops costing turns.
const (
	startLimitBurst  = 5
	startLimitWindow = 600
)

// startLimitHit is systemd's result for a unit past its start limit.
const startLimitHit = "start-limit-hit"

// Units is what the supervisor asks of systemd.
type Units interface {
	Start(unit string) error
	Stop(unit string) error
	Restart(unit string) error
	Active(unit string) (bool, error)
	// Result is systemd's word for how the unit last ended ("success",
	// "exit-code", "start-limit-hit", ...).
	Result(unit string) (string, error)
	ResetFailed(unit string) error
	Reload() error
}

// EnsureTemplate writes the unit template under dir (the user's systemd
// folder) and reloads systemd, only when the file is missing or differs:
// every daemon start calls it, and an unchanged file needs no reload.
func EnsureTemplate(dir, execPath string, paths Paths, u Units) error {
	want := []byte(templateText(execPath, paths.State))
	path := filepath.Join(dir, paths.Template())
	if have, err := os.ReadFile(path); err == nil && bytes.Equal(have, want) {
		return nil
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(path, want, 0o644); err != nil {
		return err
	}
	return u.Reload()
}

// Systemctl is Units through `systemctl --user`.
type Systemctl struct{}

func (Systemctl) run(args ...string) error {
	out, err := exec.Command("systemctl", append([]string{"--user"}, args...)...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("systemctl %s: %v: %s", strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return nil
}

func (s Systemctl) Start(unit string) error   { return s.run("start", unit) }
func (s Systemctl) Stop(unit string) error    { return s.run("stop", unit) }
func (s Systemctl) Restart(unit string) error { return s.run("restart", unit) }
func (s Systemctl) Reload() error             { return s.run("daemon-reload") }
func (s Systemctl) ResetFailed(unit string) error {
	return s.run("reset-failed", unit)
}

func (Systemctl) Result(unit string) (string, error) {
	out, err := exec.Command("systemctl", "--user", "show", "-p", "Result", "--value", unit).Output()
	if err != nil {
		return "", fmt.Errorf("systemctl show %s: %w", unit, err)
	}
	return strings.TrimSpace(string(out)), nil
}

// Active reports whether the unit is running or coming up. is-active exits
// non-zero for every other state, which is an answer, not a failure.
func (Systemctl) Active(unit string) (bool, error) {
	out, err := exec.Command("systemctl", "--user", "is-active", unit).Output()
	state := strings.TrimSpace(string(out))
	switch state {
	case "active", "activating", "reloading":
		return true, nil
	case "inactive", "failed", "deactivating", "unknown", "":
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("systemctl is-active %s: %v (%s)", unit, err, state)
	}
	return false, nil
}
