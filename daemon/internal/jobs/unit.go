package jobs

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"ccmux.dev/ccmuxd/internal/walls"
)

// Limits are the caps every job's service runs under (walls.Caps): its
// time limit, the backstop above every kind's own limit (Spec.MaxSeconds)
// for a job that no longer even notices it is late, and the memory, CPU
// and process caps a session's service has too.
type Limits = walls.Caps

// DefaultLimits are the caps of a daemon whose Settings name none
// (walls.Defaults): they only stop a job that ran away.
func DefaultLimits() Limits { return walls.Defaults().Caps() }

// ownerMark is the line of the template that says whose it is: the folder
// its jobs' files live in. It is a comment to systemd and the ownership
// record to ccmuxd. A daemon that finds another folder on this line treats
// the template as another daemon's and will not touch it.
const ownerMark = "# ccmux-job-state="

// templateText is the unit template. Each instance runs the job whose
// folder is named after it, through the daemon binary's job-exec verb.
//
// Type=exec: starting it returns once the job runs, and a job is never
// started twice at once, because a start of a running unit does nothing.
// No Restart=: whether a job that died runs again is the daemon's to
// decide, with the job's journal and a cap on tries. KillMode=control-group
// stops everything the job started with it (a check, and what the check
// started), which is what leaves no orphan behind. Nothing enables the
// instances: after a reboot the daemon starts the ones that have no result.
func templateText(execPath string, p Paths, l Limits) string {
	return fmt.Sprintf(`%s%s
[Unit]
Description=ccmux job %%i

[Service]
Type=exec
ExecStart=%s job-exec %s/%%i/spec.json
KillMode=control-group
TimeoutStopSec=20
RuntimeMaxSec=%d
%sStandardInput=null
`, ownerMark, p.State, walls.Word(execPath), walls.Word(p.State), int(l.Longest.Seconds()), l.Lines())
}

// Units is what the jobs ask of systemd.
type Units interface {
	Start(unit string) error
	Stop(unit string) error
	State(unit string) (UnitState, error)
	Reload() error
}

// UnitState is what systemd says of a unit: ActiveState ("active",
// "inactive", "failed", "activating", "deactivating", ...) and Result, its
// word for how the unit last ended ("success", "exit-code", "signal",
// "timeout", "oom-kill", ...).
type UnitState struct{ Active, Result string }

// Dead reports whether systemd says, in so many words, that the unit is
// not running and not about to: inactive or failed. Everything else (it
// runs, it is starting or stopping, systemd gave no answer) is not dead.
// Read live on this host: a unit that never ran, one that exited 0 and one
// that was stopped all read inactive, a killed one failed.
func (s UnitState) Dead() bool { return s.Active == "inactive" || s.Active == "failed" }

// EnsureTemplate writes the unit template under dir (the user's systemd
// folder) and reloads systemd, only when the file is missing or differs. A
// template that names another daemon's job folder is left alone, with an
// error that says whose it is.
func EnsureTemplate(dir, execPath string, p Paths, l Limits, u Units) error {
	want := []byte(templateText(execPath, p, l))
	path := filepath.Join(dir, p.Template())
	have, err := os.ReadFile(path)
	if err == nil && bytes.Equal(have, want) {
		return nil
	}
	if err == nil {
		if other := ownerOf(have); other != p.State {
			return fmt.Errorf("the unit template %s belongs to another daemon (its jobs live in %s): pass -job-unit-prefix to give this one its own, or if that template is a leftover, remove %s and restart ccmuxd", p.Template(), other, path)
		}
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(path, want, 0o644); err != nil {
		return err
	}
	return u.Reload()
}

// ownerOf is the job folder a template's text names, "(none named)" for a
// file without the mark.
func ownerOf(text []byte) string {
	for _, line := range strings.Split(string(text), "\n") {
		if owner, ok := strings.CutPrefix(line, ownerMark); ok {
			return owner
		}
	}
	return "(none named)"
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

func (s Systemctl) Start(unit string) error { return s.run("start", unit) }
func (s Systemctl) Reload() error           { return s.run("daemon-reload") }

// Stop asks for the unit to be stopped and does not wait for it: a job's
// processes get the unit's stop timeout to go, and whoever asked reads the
// unit's state to see that they have.
func (s Systemctl) Stop(unit string) error { return s.run("stop", "--no-block", unit) }

// State asks systemd for the unit's state. A systemctl that fails, or that
// answers without the state, is an error and never a state: "I could not
// ask" must not read as "it is dead".
func (Systemctl) State(unit string) (UnitState, error) {
	out, err := exec.Command("systemctl", "--user", "show", "-p", "ActiveState", "-p", "Result", unit).Output()
	if err != nil {
		return UnitState{}, fmt.Errorf("systemctl show %s: %w", unit, err)
	}
	st := parseUnitState(string(out))
	if st.Active == "" {
		return UnitState{}, fmt.Errorf("systemctl show %s: no ActiveState in its answer", unit)
	}
	return st, nil
}

func parseUnitState(out string) UnitState {
	var st UnitState
	for _, line := range strings.Split(out, "\n") {
		if v, ok := strings.CutPrefix(line, "ActiveState="); ok {
			st.Active = strings.TrimSpace(v)
		}
		if v, ok := strings.CutPrefix(line, "Result="); ok {
			st.Result = strings.TrimSpace(v)
		}
	}
	return st
}
