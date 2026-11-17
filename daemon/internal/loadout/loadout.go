// Package loadout decides what a thread session (a runner session: a repo's
// lead, a thread, a thread's helper) loads from the capability catalog
// (agent/capabilities.go, agent/loadout.go). A group (a shared window)
// switches catalog entries on; a repo (a workspace) follows its group, or
// switches one entry on or off for itself. A session loads what its own
// repo's workspace comes to, so a thread's helper follows the repo it works
// in, not its lead's.
//
// Choices apply when a session next starts. The session's row keeps what it
// started with, and a session whose list has moved on since is "out of
// date": the page offers a restart, nothing restarts by itself.
//
// Terminal panes, task builders and isolated agents are not sessions of
// this kind and load what they always did.
package loadout

import (
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"ccmux.dev/ccmuxd/internal/agent"
	"ccmux.dev/ccmuxd/internal/store"
)

// Store is the part of the registry the service reads and writes
// (store/capchoices.go).
type Store interface {
	CapabilityChoices() ([]store.CapChoice, error)
	CapabilityChoicesSeeded() (bool, error)
	SeedCapabilityChoices(rows []store.CapChoice, now int64) (bool, error)
	WindowMembers() (map[string]string, error)
	AllWindows() (map[string]string, error)
}

// Catalog is the capability catalog (*agent.Store).
type Catalog interface {
	Capabilities() ([]agent.Capability, error)
	Sources() ([]agent.CapSource, error)
	ImportCapabilities(claudeDir string, probe *agent.ProbeResult) (agent.ImportResult, error)
	LoadoutFor(ids []string, claudeDir string, folders ...string) (agent.Loadout, agent.LoadoutRecord, error)
}

// Repo is one repo session (workspace) on this daemon, never an agent's.
type Repo struct {
	WorkspaceID string
	Path        string
}

// Service resolves loadouts on this daemon.
type Service struct {
	St      Store
	Catalog Catalog
	// ClaudeDir is the host user's Claude config folder (agent.ClaudeConfigDir).
	ClaudeDir func() (string, error)
	// Repos lists the repo sessions, for the day-one choices (Seed).
	Repos func() []Repo
	Now   func() time.Time

	seeding sync.Mutex // one Seed at a time
}

// Effective is the catalog ids on for a repo: what its group has on, less
// what the repo switches off, plus what the repo switches on. Sorted.
func Effective(group, repo map[string]string) []string {
	out := []string{}
	for id, state := range group {
		if state == store.ChoiceOn && repo[id] != store.ChoiceOff {
			out = append(out, id)
		}
	}
	for id, state := range repo {
		if state == store.ChoiceOn && group[id] != store.ChoiceOn {
			out = append(out, id)
		}
	}
	sort.Strings(out)
	return out
}

// Snapshot is the choices and the catalog read once, for answering about
// many workspaces or sessions in one go (the session list asks per row).
type Snapshot struct {
	catalog []agent.Capability
	members map[string]string            // workspace id → window id
	choices map[string]map[string]string // scope + "/" + scope id → cap id → state
}

// Snapshot reads the choices and the catalog as they are now.
func (s *Service) Snapshot() (*Snapshot, error) {
	catalog, err := s.Catalog.Capabilities()
	if err != nil {
		return nil, err
	}
	members, err := s.St.WindowMembers()
	if err != nil {
		return nil, err
	}
	all, err := s.St.CapabilityChoices()
	if err != nil {
		return nil, err
	}
	snap := &Snapshot{catalog: catalog, members: members, choices: map[string]map[string]string{}}
	for _, c := range all {
		key := c.Scope + "/" + c.ScopeID
		if snap.choices[key] == nil {
			snap.choices[key] = map[string]string{}
		}
		snap.choices[key][c.CapID] = c.State
	}
	return snap, nil
}

// Window is the id of the group a workspace is in, "" for one in none.
func (n *Snapshot) Window(workspaceID string) string { return n.members[workspaceID] }

// Group is what a window has on, cap id → "on". Never nil.
func (n *Snapshot) Group(windowID string) map[string]string {
	return orEmpty(n.choices[store.ChoiceWindow+"/"+windowID])
}

// Repo is a workspace's own choices, cap id → "on" or "off". Never nil.
func (n *Snapshot) Repo(workspaceID string) map[string]string {
	return orEmpty(n.choices[store.ChoiceWorkspace+"/"+workspaceID])
}

func orEmpty(m map[string]string) map[string]string {
	if m == nil {
		return map[string]string{}
	}
	return m
}

// Members are the workspaces in a window, sorted.
func (n *Snapshot) Members(windowID string) []string {
	out := []string{}
	for ws, win := range n.members {
		if win == windowID {
			out = append(out, ws)
		}
	}
	sort.Strings(out)
	return out
}

// All is every choice there is: per window the ids it has on, per workspace
// its own on and off.
func (n *Snapshot) All() (windows map[string][]string, workspaces map[string]map[string]string) {
	windows, workspaces = map[string][]string{}, map[string]map[string]string{}
	for key, choices := range n.choices {
		scope, id, _ := strings.Cut(key, "/")
		if scope == store.ChoiceWindow {
			windows[id] = Effective(choices, nil)
		} else {
			workspaces[id] = choices
		}
	}
	return windows, workspaces
}

// IDs is what is on for a workspace. A session in no workspace has no
// group and no repo, so nothing is on for it.
func (n *Snapshot) IDs(workspaceID string) []string {
	if workspaceID == "" {
		return []string{}
	}
	var group map[string]string
	if win := n.Window(workspaceID); win != "" {
		group = n.Group(win)
	}
	return Effective(group, n.Repo(workspaceID))
}

// Missing are the ids on for a workspace that the catalog no longer holds:
// a session skips them and says so.
func (n *Snapshot) Missing(workspaceID string) []string {
	out := []string{}
	for _, id := range n.IDs(workspaceID) {
		if !n.Has(id) {
			out = append(out, id)
		}
	}
	return out
}

// Has says whether the catalog holds id.
func (n *Snapshot) Has(id string) bool {
	return slices.ContainsFunc(n.catalog, func(c agent.Capability) bool { return c.ID == id })
}

// Stale says whether a session runs on a list that has moved on: what is on
// for its repo, or a version of something it loaded, is no longer what it
// started with. Only a session that should be running can be: one that is
// asleep or ended reads its list fresh at its next start. A running session
// with no record started before loadouts did and still has everything.
func (n *Snapshot) Stale(sess store.RunnerSession) bool {
	if sess.Wanted != store.SessionRunning {
		return false
	}
	rec, ok := Record(sess)
	if !ok {
		return true
	}
	return rec.Signature != agent.LoadoutSignature(n.catalog, n.IDs(sess.WorkspaceID))
}

// Record is the loadout a session's row says it started with; false when
// it has none (or one that does not parse, which is the same to a reader).
func Record(sess store.RunnerSession) (agent.LoadoutRecord, bool) {
	var rec agent.LoadoutRecord
	if sess.Loadout == "" || json.Unmarshal([]byte(sess.Loadout), &rec) != nil {
		return rec, false
	}
	return rec, true
}

// ErrNotSeeded refuses a start before the day-one choices are written: a
// session started then would get nothing its repo uses today, the commit
// gates among it, and nobody would see that it had.
var ErrNotSeeded = errors.New("skills and plugins: the day-one choices could not be written, so this session would start without its repo's plugins")

// ForSession resolves the loadout of a session being started now: the file
// its runner reads and the record its row keeps (runner.Config.Loadout).
func (s *Service) ForSession(sess store.RunnerSession) (file []byte, record string, err error) {
	// The daemon seeds at its start; a seed that failed then (a file that
	// did not read) is tried again here, so fixing the cause is enough.
	if err := s.Seed(); err != nil {
		return nil, "", fmt.Errorf("%w: %v", ErrNotSeeded, err)
	}
	snap, err := s.Snapshot()
	if err != nil {
		return nil, "", err
	}
	claudeDir, err := s.ClaudeDir()
	if err != nil {
		return nil, "", err
	}
	folders := []string{sess.Cwd}
	if sess.Repo != "" {
		folders = append(folders, sess.Repo)
	}
	lo, rec, err := s.Catalog.LoadoutFor(snap.IDs(sess.WorkspaceID), claudeDir, folders...)
	if err != nil {
		return nil, "", fmt.Errorf("skills and plugins: %w", err)
	}
	rec.StartedAt = s.Now().UnixMilli()
	file, err = json.MarshalIndent(lo, "", "  ")
	if err != nil {
		return nil, "", err
	}
	blob, err := json.Marshal(rec)
	return file, string(blob), err
}
