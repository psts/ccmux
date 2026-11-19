package walls

import (
	"errors"
	"fmt"
	"runtime"
	"strings"
	"time"
)

// Settings is what a person chose on the Sandbox tab of Settings: whether
// units are walled, and the caps every session and job service runs under.
type Settings struct {
	// On walls the units that do task and thread work. Off by default: a
	// host has to allow walls first (Host).
	On bool `json:"on"`
	// MemoryPercent is the most of the machine's memory one unit may use.
	MemoryPercent int `json:"memoryPercent"`
	// CPUCores is how many cores' worth of CPU one unit may use.
	CPUCores int `json:"cpuCores"`
	// TasksMax is how many processes and threads one unit may have.
	TasksMax int `json:"tasksMax"`
	// JobHours is how long one job may run before its service stops it.
	JobHours int `json:"jobHours"`
	// ExtraWrite are folders a walled unit may write besides its own: a
	// cache this package does not know (walls.Caches is the list it does).
	ExtraWrite []string `json:"extraWrite"`
	// ShellBox also runs a walled session's shell commands in Claude Code's
	// own sandbox. A switch of its own, off by default, because of what it
	// costs (seen live, 2026-11-19): a sandboxed command has a network of
	// its own, so it reaches no loopback address (not the session's own dev
	// servers, not this daemon) and only the hosts the person's Claude
	// settings allow (sandbox.network.allowedDomains), where a builder's
	// checks fetch from wherever the repo says.
	ShellBox bool `json:"shellBox"`
}

// The bounds a setting is kept in.
const (
	minMemoryPercent, maxMemoryPercent = 10, 100
	minTasks, maxTasks                 = 256, 1 << 20
	minJobHours, maxJobHours           = 1, 48
	maxExtraWrite                      = 32
)

// Defaults are caps that only stop a unit that ran away: three quarters of
// the machine's memory, every core but one, far more processes than a test
// run starts, and six hours for a job. Work under them runs as it does
// without them, unless it alone would have starved everything else on the
// host. They apply to every unit at once when they change, the running
// ones too (systemd re-reads them), which is why they are this loose.
func Defaults() Settings {
	return Settings{MemoryPercent: 75, CPUCores: maxCores(runtime.NumCPU() - 1), TasksMax: 8192, JobHours: 6, ExtraWrite: []string{}}
}

func maxCores(n int) int {
	if n < 1 {
		return 1
	}
	return n
}

// ErrBadSettings is a setting out of its bounds.
var ErrBadSettings = errors.New("sandbox settings out of range")

// Validate says what is wrong with the settings, nil for nothing.
func (s Settings) Validate() error {
	bad := func(format string, args ...any) error {
		return fmt.Errorf("%w: %s", ErrBadSettings, fmt.Sprintf(format, args...))
	}
	for _, n := range []struct {
		what     string
		is       int
		min, max int
	}{
		{"memory, in percent", s.MemoryPercent, minMemoryPercent, maxMemoryPercent},
		{"CPU, in cores (this host has " + fmt.Sprint(runtime.NumCPU()) + ")", s.CPUCores, 1, runtime.NumCPU()},
		{"processes", s.TasksMax, minTasks, maxTasks},
		{"a job's hours", s.JobHours, minJobHours, maxJobHours},
	} {
		if n.is < n.min || n.is > n.max {
			return bad("%s is %d, and must be %d to %d", n.what, n.is, n.min, n.max)
		}
	}
	if len(s.ExtraWrite) > maxExtraWrite {
		return bad("%d extra folders, and at most %d fit", len(s.ExtraWrite), maxExtraWrite)
	}
	for _, path := range s.ExtraWrite {
		if err := ValidPath(path); err != nil {
			return bad("%v", err)
		}
	}
	return nil
}

// Caps are the limits one unit's service runs under, in systemd's words.
type Caps struct {
	// Longest is a job's time limit (RuntimeMaxSec); a session has none.
	Longest time.Duration
	// CPUPercent is CPUQuota, in percent of one core.
	CPUPercent int
	MemoryMax  string
	TasksMax   int
}

// Caps are the settings as a unit file states them.
func (s Settings) Caps() Caps {
	return Caps{
		Longest:    time.Duration(s.JobHours) * time.Hour,
		CPUPercent: s.CPUCores * 100,
		MemoryMax:  fmt.Sprintf("%d%%", s.MemoryPercent),
		TasksMax:   s.TasksMax,
	}
}

// Lines are the unit file lines of the caps that hold for a session and a
// job alike. NoNewPrivileges keeps a unit's processes from gaining rights
// through a setuid program (sudo, and the like), walls or no walls.
func (c Caps) Lines() string {
	var b strings.Builder
	fmt.Fprintf(&b, "MemoryMax=%s\nCPUQuota=%d%%\nTasksMax=%d\nNoNewPrivileges=yes\n", c.MemoryMax, c.CPUPercent, c.TasksMax)
	return b.String()
}
