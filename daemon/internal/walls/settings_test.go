package walls

import (
	"errors"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestTheDefaultsAreValidAndOff(t *testing.T) {
	d := Defaults()
	if err := d.Validate(); err != nil {
		t.Fatal(err)
	}
	if d.On {
		t.Fatal("the sandbox is off until a person switches it on")
	}
	if d.CPUCores < 1 || d.CPUCores > runtime.NumCPU() {
		t.Fatalf("cores: %d", d.CPUCores)
	}
}

func TestSettingsOutOfBoundsAreRefused(t *testing.T) {
	for name, change := range map[string]func(*Settings){
		"no memory":       func(s *Settings) { s.MemoryPercent = 5 },
		"too much memory": func(s *Settings) { s.MemoryPercent = 101 },
		"no cores":        func(s *Settings) { s.CPUCores = 0 },
		"too many cores":  func(s *Settings) { s.CPUCores = runtime.NumCPU() + 1 },
		"too few tasks":   func(s *Settings) { s.TasksMax = 10 },
		"no hours":        func(s *Settings) { s.JobHours = 0 },
		"a relative path": func(s *Settings) { s.ExtraWrite = []string{"cache"} },
		"a unit line":     func(s *Settings) { s.ExtraWrite = []string{"/a\nProtectHome=no"} },
		"under /tmp":      func(s *Settings) { s.ExtraWrite = []string{"/tmp/x"} },
	} {
		s := Defaults()
		change(&s)
		if err := s.Validate(); !errors.Is(err, ErrBadSettings) {
			t.Errorf("%s: want ErrBadSettings, got %v", name, err)
		}
	}
}

func TestCapsAreWhatAUnitFileSays(t *testing.T) {
	c := Settings{MemoryPercent: 60, CPUCores: 2, TasksMax: 4096, JobHours: 3}.Caps()
	if c.Longest != 3*time.Hour {
		t.Errorf("longest: %s", c.Longest)
	}
	lines := c.Lines()
	for _, want := range []string{"MemoryMax=60%\n", "CPUQuota=200%\n", "TasksMax=4096\n", "NoNewPrivileges=yes\n"} {
		if !strings.Contains(lines, want) {
			t.Errorf("caps miss %q: %q", want, lines)
		}
	}
}
