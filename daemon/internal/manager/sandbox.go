package manager

import (
	"encoding/json"
	"errors"
	"fmt"

	"ccmux.dev/ccmuxd/internal/walls"
)

// settingSandbox holds the Sandbox tab's choices (walls.Settings) as JSON:
// whether task and thread work is walled, and the caps every session and
// job service runs under. Unset, the defaults hold (walls.Defaults).
const settingSandbox = "sandbox"

// Sandbox returns the sandbox settings, the defaults for a daemon that
// never saved any. A read error is the caller's to act on: a unit is then
// not started rather than started open on a guess that the sandbox is off.
func (m *Manager) Sandbox() (walls.Settings, error) {
	set := walls.Defaults()
	if m.store == nil {
		return set, nil
	}
	v, err := m.store.GetSetting(settingSandbox)
	if err != nil || v == "" {
		return set, err
	}
	// Over the defaults: a setting added later reads as its default in a
	// row saved before it.
	if err := json.Unmarshal([]byte(v), &set); err != nil {
		return walls.Defaults(), fmt.Errorf("stored sandbox settings: %w", err)
	}
	if set.ExtraWrite == nil {
		set.ExtraWrite = []string{}
	}
	return set, nil
}

// SetSandbox stores the sandbox settings.
func (m *Manager) SetSandbox(set walls.Settings) error {
	if err := set.Validate(); err != nil {
		return err
	}
	if m.store == nil {
		return errors.New("no store: settings are not available")
	}
	if set.ExtraWrite == nil {
		set.ExtraWrite = []string{}
	}
	b, err := json.Marshal(set)
	if err != nil {
		return err
	}
	return m.store.SetSetting(settingSandbox, string(b))
}
