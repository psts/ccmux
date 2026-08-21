package manager

import (
	"errors"
	"fmt"
	"log"
	"sort"
	"strings"

	"github.com/google/uuid"
)

// Shared windows (v2): ONE arrangement for everyone. A window is a shared
// entity (name + member workspaces, a workspace in at most one window); the
// only personal state is which windows each login has OPEN. The rows live in
// this daemon's store, but the daemon that USES them is the one lenses talk
// to — the hub in a federation, a lone daemon otherwise. Membership may
// reference remote workspace ids the local manager has never heard of; the
// API layer validates existence against the surface it serves.
// See docs/multitenant-plan.md ("v2: shared windows").

// WindowInfo is one shared window as lenses see it (GET /v1/windows).
type WindowInfo struct {
	ID           string   `json:"id"`
	Name         string   `json:"name"`
	WorkspaceIDs []string `json:"workspaceIds"`
	// OpenBy lists the logins that currently have the window open. The API
	// stamps the caller-relative Open flag from it.
	OpenBy []string `json:"openBy"`
}

// windowState is one coherent snapshot of the shared-window tables plus the
// legacy-import markers (nil = unreadable, which BLOCKS imports). Cached so
// group resolution under the peers bus mutex never touches the store — the
// same rule hub/aggregator.go states for its own maps.
type windowState struct {
	names    map[string]string          // window id → name
	members  map[string]string          // ws id → window id
	opens    map[string]map[string]bool // window id → logins with it open
	imported map[string]bool
}

// resolve is the group ladder: shared membership wins; the legacy persisted
// group stands until the import ran; after import, no membership means "" —
// ungrouped, and the peers bus falls to its directory fallback.
func (st *windowState) resolve(wsID, legacy string) string {
	if wid, ok := st.members[wsID]; ok {
		return st.names[wid]
	}
	if st.imported[wsID] {
		return ""
	}
	return legacy
}

// windowSnapshot returns the cached state, rebuilding from the store only
// after a write invalidated it. A snapshot built from a failed read is served
// but NOT cached, so the next call retries instead of freezing the failure.
func (m *Manager) windowSnapshot() *windowState {
	m.viewsMu.Lock()
	defer m.viewsMu.Unlock()
	if m.windowCache != nil {
		return m.windowCache
	}
	st := &windowState{
		names: map[string]string{}, members: map[string]string{},
		opens: map[string]map[string]bool{}, imported: map[string]bool{},
	}
	if m.store == nil {
		m.windowCache = st
		return st
	}
	var errs []error
	read := func(name string, f func() error) {
		if err := f(); err != nil {
			log.Printf("windows: reading %s failed (%v); serving without", name, err)
			errs = append(errs, err)
		}
	}
	read("windows", func() (err error) { st.names, err = m.store.AllWindows(); return })
	read("membership", func() (err error) { st.members, err = m.store.WindowMembers(); return })
	read("open flags", func() (err error) { st.opens, err = m.store.WindowOpens(); return })
	if imported, err := m.store.ViewImports(); err != nil {
		log.Printf("windows: reading import markers failed (%v); skipping legacy import this pass", err)
		st.imported = nil
	} else {
		st.imported = imported
	}
	if len(errs) == 0 && st.imported != nil {
		m.windowCache = st
	}
	return st
}

// invalidateWindows drops the cached snapshot; every window write calls it.
func (m *Manager) invalidateWindows() {
	m.viewsMu.Lock()
	m.windowCache = nil
	m.viewsMu.Unlock()
}

// SharedGroupResolver returns the group ladder over one cached snapshot —
// safe to call under the peers bus mutex.
func (m *Manager) SharedGroupResolver() func(wsID, legacy string) string {
	return m.windowSnapshot().resolve
}

// Windows lists every shared window, sorted by name.
func (m *Manager) Windows() []WindowInfo {
	st := m.windowSnapshot()
	byID := map[string]*WindowInfo{}
	for id, name := range st.names {
		byID[id] = &WindowInfo{ID: id, Name: name, WorkspaceIDs: []string{}, OpenBy: []string{}}
	}
	for ws, wid := range st.members {
		if w := byID[wid]; w != nil {
			w.WorkspaceIDs = append(w.WorkspaceIDs, ws)
		}
	}
	for wid, logins := range st.opens {
		if w := byID[wid]; w != nil {
			for login := range logins {
				w.OpenBy = append(w.OpenBy, login)
			}
		}
	}
	out := make([]WindowInfo, 0, len(byID))
	for _, w := range byID {
		sort.Strings(w.WorkspaceIDs)
		sort.Strings(w.OpenBy)
		out = append(out, *w)
	}
	sort.Slice(out, func(i, j int) bool { return strings.ToLower(out[i].Name) < strings.ToLower(out[j].Name) })
	return out
}

// WindowByName finds a shared window case-insensitively (names are unique
// under that rule; EnsureWindow enforces it).
func (m *Manager) WindowByName(name string) (string, bool) {
	for id, n := range m.windowSnapshot().names {
		if strings.EqualFold(n, name) {
			return id, true
		}
	}
	return "", false
}

// EnsureWindow finds or creates the window of that name.
func (m *Manager) EnsureWindow(name string) (string, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return "", errors.New("a window needs a name")
	}
	if id, ok := m.WindowByName(name); ok {
		return id, nil
	}
	if m.store == nil {
		return "", errors.New("no store: windows are not available")
	}
	id := uuid.NewString()
	if err := m.store.CreateWindow(id, name); err != nil {
		return "", err
	}
	m.invalidateWindows()
	return id, nil
}

// AssignWorkspace puts a workspace in the window of that name (creating the
// window if new); an empty name removes it from any window. This is a SHARED
// edit — every lens sees it — broadcast as workspace-status.
func (m *Manager) AssignWorkspace(wsID, windowName string) error {
	if m.store == nil {
		return errors.New("no store: windows are not available")
	}
	windowName = strings.TrimSpace(windowName)
	if windowName == "" {
		if err := m.store.RemoveWindowMember(wsID); err != nil {
			return err
		}
	} else {
		wid, err := m.EnsureWindow(windowName)
		if err != nil {
			return err
		}
		if err := m.store.SetWindowMember(wsID, wid); err != nil {
			return err
		}
	}
	m.invalidateWindows()
	m.events.publish(Event{Kind: "workspace-status", WorkspaceID: wsID})
	return nil
}

// SeedWindowMembership is AssignWorkspace plus the import marker — the create
// and revive paths seed through it so the compat-persisted legacy group can
// never resurrect a deliberately removed membership.
func (m *Manager) SeedWindowMembership(wsID, windowName string) error {
	if m.store == nil {
		return errors.New("no store: windows are not available")
	}
	if err := m.store.MarkViewImported(wsID); err != nil {
		return err
	}
	return m.AssignWorkspace(wsID, windowName)
}

// SetWindowOpen records one login's open/close of a window. On a close it
// reports whether that was the LAST opener — the lens then archives the
// members (the agreed model: nobody has it open, the window goes to sleep) —
// along with the member workspace ids.
func (m *Manager) SetWindowOpen(login, windowID string, open bool) (last bool, members []string, err error) {
	if m.store == nil {
		return false, nil, errors.New("no store: windows are not available")
	}
	if err := m.store.SetWindowOpen(login, windowID, open); err != nil {
		return false, nil, err
	}
	m.invalidateWindows()
	st := m.windowSnapshot()
	if _, known := st.names[windowID]; !known {
		return false, nil, fmt.Errorf("unknown window %s", windowID)
	}
	if !open && len(st.opens[windowID]) == 0 {
		last = true
		for ws, wid := range st.members {
			if wid == windowID {
				members = append(members, ws)
			}
		}
		sort.Strings(members)
	}
	m.events.publish(Event{Kind: "workspace-status"})
	return last, members, nil
}

// RenameSharedWindow renames, refusing a case-insensitive collision with
// another window (names are the human handle; two windows answering to one
// name would merge in every lens).
func (m *Manager) RenameSharedWindow(id, name string) error {
	if m.store == nil {
		return errors.New("no store: windows are not available")
	}
	name = strings.TrimSpace(name)
	if name == "" {
		return errors.New("a window needs a name")
	}
	if other, ok := m.WindowByName(name); ok && other != id {
		return fmt.Errorf("a window named %q already exists", name)
	}
	if err := m.store.RenameWindow(id, name); err != nil {
		return err
	}
	m.invalidateWindows()
	m.events.publish(Event{Kind: "workspace-status"})
	return nil
}

// WindowsStrict reads membership, open flags, and names straight from the
// store for callers that must not mistake an unreadable table for an empty
// one — the archive guard fails CLOSED on the error.
func (m *Manager) WindowsStrict() (members map[string]string, opens map[string]map[string]bool, names map[string]string, err error) {
	if m.store == nil {
		return map[string]string{}, map[string]map[string]bool{}, map[string]string{}, nil
	}
	if members, err = m.store.WindowMembers(); err != nil {
		return nil, nil, nil, err
	}
	if opens, err = m.store.WindowOpens(); err != nil {
		return nil, nil, nil, err
	}
	if names, err = m.store.AllWindows(); err != nil {
		return nil, nil, nil, err
	}
	return members, opens, names, nil
}

// ViewImports returns which workspaces already ran their legacy-group import;
// nil means the markers are unreadable and imports must not run.
func (m *Manager) ViewImports() map[string]bool {
	return m.windowSnapshot().imported
}

const settingWindowsMigrated = "windows_migrated"

// MigrateViewsToWindows converts the retired per-login view rows into shared
// windows, once: distinct names (case-insensitively merged) become windows,
// each workspace joins the window its lexicographically-first row names (the
// per-login model has no better tiebreak), and every row becomes an open flag
// for its login. A wiped views table migrates to nothing — the clean start.
func (m *Manager) MigrateViewsToWindows() error {
	if m.store == nil {
		return nil
	}
	if m.getSetting(settingWindowsMigrated) == "1" {
		return nil
	}
	views, err := m.store.AllViews()
	if err != nil {
		return err
	}
	for ws, rows := range views {
		logins := make([]string, 0, len(rows))
		for login := range rows {
			logins = append(logins, login)
		}
		sort.Strings(logins)
		for i, login := range logins {
			wid, err := m.EnsureWindow(rows[login])
			if err != nil {
				return err
			}
			if i == 0 {
				if err := m.store.SetWindowMember(ws, wid); err != nil {
					return err
				}
			}
			if err := m.store.SetWindowOpen(login, wid, true); err != nil {
				return err
			}
		}
	}
	m.invalidateWindows()
	return m.store.SetSetting(settingWindowsMigrated, "1")
}
