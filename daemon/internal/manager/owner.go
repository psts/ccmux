package manager

import "strings"

// The host owner is the tailnet login (an email) of the human this machine
// belongs to. It exists because a caller on the machine itself arrives over
// loopback, where WhoIs declines — so without it, the person most likely to be
// using this host is the one person the daemon cannot name. resolveIdentity
// uses it as the fallback tier between "verified by WhoIs" and "self-declared",
// and the hub uses it to attribute this host's sessions to their owner
// (see docs/multitenant-plan.md).

const settingOwner = "owner"

// Owner returns the configured host-owner login ("" when unset). Nil-safe:
// /v1/health reports it, and health must answer even on a bare Server.
func (m *Manager) Owner() string {
	if m == nil || m.store == nil {
		return ""
	}
	return m.getSetting(settingOwner)
}

// SetOwner persists the host-owner login; empty clears it.
func (m *Manager) SetOwner(login string) error {
	return m.store.SetSetting(settingOwner, strings.TrimSpace(login))
}
