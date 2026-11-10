package manager

import (
	"errors"
	"fmt"
	"strconv"
)

// settingTaskSlotsTotal is the most queued tasks built at once across every
// window ("" or 0 for no cap), on top of each window's own number.
const settingTaskSlotsTotal = "task_slots_total"

// MaxTaskSlotsTotal bounds the cap the Settings picker offers.
const MaxTaskSlotsTotal = 6

// ErrBadTaskSlotsTotal marks a cap off the picker.
var ErrBadTaskSlotsTotal = errors.New("task cap across windows out of range")

// TaskSlotsTotal returns the cap across windows, 0 for none. A read error is
// the caller's to act on: the runner then starts nothing new rather than
// run with no cap.
func (m *Manager) TaskSlotsTotal() (int, error) {
	if m.store == nil {
		return 0, nil
	}
	v, err := m.store.GetSetting(settingTaskSlotsTotal)
	if err != nil || v == "" {
		return 0, err
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return 0, fmt.Errorf("stored task cap %q: %w", v, err)
	}
	return n, nil
}

// SetTaskSlotsTotal stores the cap across windows; 0 removes it.
func (m *Manager) SetTaskSlotsTotal(n int) error {
	if err := ValidateTaskSlotsTotal(n); err != nil {
		return err
	}
	if m.store == nil {
		return errors.New("no store: settings are not available")
	}
	return m.store.SetSetting(settingTaskSlotsTotal, strconv.Itoa(n))
}

// ValidateTaskSlotsTotal refuses a cap off the picker (0 to MaxTaskSlotsTotal).
func ValidateTaskSlotsTotal(n int) error {
	if n < 0 || n > MaxTaskSlotsTotal {
		return fmt.Errorf("%w: %d (0 for none, up to %d)", ErrBadTaskSlotsTotal, n, MaxTaskSlotsTotal)
	}
	return nil
}
