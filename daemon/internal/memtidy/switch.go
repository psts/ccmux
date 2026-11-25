package memtidy

import (
	"fmt"
	"log"
	"strconv"
	"strings"
	"time"
)

// Settings is where the switches are kept: the daemon's settings table.
type Settings interface {
	GetSetting(key string) (string, error)
	SetSetting(key, value string) error
}

// Switches are the per-repo on/off switches of the nightly tidy-up. A repo
// is on unless it was switched off. The setting is "off", or "on <unix
// ms>" for a repo switched on again: a night reads no further back than
// firstLook before that, so a repo off for months does not read months.
// The time is in the setting, not in the bookmarks, because the bookmarks
// are the night's own file and a switch flipped while it runs would race it.
type Switches struct {
	Settings Settings
	// Key is the repo a folder belongs to (its main folder, todos.Keys),
	// so a worktree never has a switch of its own.
	Key func(path string) (string, error)
	// StateDir is the tidy-up's state folder, which has the log.
	StateDir string
	now      func() time.Time
}

const switchPrefix = "memory-tidy:"

// Switch is one repo's switch as it is now.
type Switch struct {
	On bool
	// OnAt is when it was switched on again; zero when never switched off.
	OnAt time.Time
}

func (s *Switches) key(path string) (string, error) {
	if s.Key == nil {
		return path, nil
	}
	return s.Key(path)
}

// State reads the repo's switch. An error is never taken as on or off.
func (s *Switches) State(path string) (Switch, error) {
	key, err := s.key(path)
	if err != nil {
		return Switch{}, err
	}
	v, err := s.Settings.GetSetting(switchPrefix + key)
	if err != nil {
		return Switch{}, err
	}
	return parseSwitch(v)
}

func parseSwitch(v string) (Switch, error) {
	switch {
	case v == "":
		return Switch{On: true}, nil
	case v == "off":
		return Switch{}, nil
	case strings.HasPrefix(v, "on "):
		ms, err := strconv.ParseInt(strings.TrimPrefix(v, "on "), 10, 64)
		if err != nil {
			return Switch{}, fmt.Errorf("the switch reads %q", v)
		}
		return Switch{On: true, OnAt: time.UnixMilli(ms)}, nil
	}
	return Switch{}, fmt.Errorf("the switch reads %q", v)
}

// Set switches a repo on or off. Switching on a repo that is on changes
// nothing, so its time stays the one it was switched on at.
func (s *Switches) Set(path string, on bool) error {
	cur, err := s.State(path)
	if err != nil {
		return err
	}
	if cur.On == on {
		return nil
	}
	key, err := s.key(path)
	if err != nil {
		return err
	}
	v := "off"
	if on {
		v = "on " + strconv.FormatInt(s.clock().UnixMilli(), 10)
	}
	return s.Settings.SetSetting(switchPrefix+key, v)
}

// Enabled is the Tidier's look at a repo's switch. A switch that cannot be
// read skips the repo tonight: on is never guessed.
func (s *Switches) Enabled(path string) (bool, time.Time) {
	sw, err := s.State(path)
	if err != nil {
		log.Printf("memory tidy: %s: its switch could not be read, so it is skipped tonight: %v", path, err)
		return false, time.Time{}
	}
	return sw.On, sw.OnAt
}

// Last is the repo's newest night in the log; nil when it has none.
func (s *Switches) Last(path string) (*Night, error) {
	key, err := s.key(path)
	if err != nil {
		return nil, err
	}
	return LastNight(s.StateDir, key, path)
}

func (s *Switches) clock() time.Time {
	if s.now != nil {
		return s.now()
	}
	return time.Now()
}
