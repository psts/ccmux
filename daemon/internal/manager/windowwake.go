package manager

import "sort"

// A window comes back as it was left. When a lens closes a window it says
// which of its sessions are awake, and those carry a wake mark (store
// session_wake); opening the window hands the marks back, and the lens wakes
// the marked sessions that are asleep. Waking a session clears its mark.
// Reading uses nothing up, so these have no state to lose between calls.

// MarkWindowWake replaces the wake marks of a window's members with awake.
func (m *Manager) MarkWindowWake(windowID string, awake []string) error {
	if m.store == nil {
		return nil
	}
	members, err := m.membersOf(windowID)
	if err != nil {
		return err
	}
	return m.store.SetWakeMarks(members, awake)
}

// WindowWake is the window's members that carry a wake mark.
func (m *Manager) WindowWake(windowID string) ([]string, error) {
	if m.store == nil {
		return []string{}, nil
	}
	members, err := m.membersOf(windowID)
	if err != nil {
		return nil, err
	}
	return m.store.WakeMarks(members)
}

// ClearWakeMark drops a session's wake mark: it has been woken.
func (m *Manager) ClearWakeMark(wsID string) error {
	if m.store == nil {
		return nil
	}
	return m.store.ClearWakeMark(wsID)
}

// membersOf is the ids of the sessions in a window, sorted, read from the
// store (windowMembers, tasks.go) rather than the cache.
func (m *Manager) membersOf(windowID string) ([]string, error) {
	memberOf, err := m.windowMembers()
	if err != nil {
		return nil, err
	}
	out := []string{}
	for ws, wid := range memberOf {
		if wid == windowID {
			out = append(out, ws)
		}
	}
	sort.Strings(out)
	return out, nil
}
