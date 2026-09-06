// Package agent holds base agent definitions: role agents ("x-poster",
// "kb-writer") that are created once and then added to projects. A base is a
// FOLDER of plain files every harness can read — AGENTS.md for the role,
// SKILL.md folders, an MCP list, reference files — plus agent.json for the
// fields only ccmux cares about (icon, harness, lifecycle, caps). The folder
// is the source of truth; there is no database row. Layout and the reasoning
// behind every field: docs/agent-spec.md.
//
// The package writes generated files and write-once seeds, and never rewrites
// a file a human may have edited: AGENTS.md only on an explicit save with new
// text, mcp.json and the instance starters only when absent. Skills, knowledge
// and everything a human or the agent put there are never touched.
package agent

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// Permissions is the harness-neutral permission vocabulary; each value is
// "allow", "ask" or "deny", and BashAllow lists command patterns that run
// without asking even when Bash is "ask". Adapters translate it per harness.
type Permissions struct {
	Read      string   `json:"read"`
	Edit      string   `json:"edit"`
	Bash      string   `json:"bash"`
	Webfetch  string   `json:"webfetch"`
	BashAllow []string `json:"bashAllow,omitempty"`
}

// Definition is agent.json plus the two fields read from beside it: Version
// from the plugin manifest and Instructions from AGENTS.md. It is what the
// API round-trips.
type Definition struct {
	Name        string `json:"name"`
	Icon        string `json:"icon"`
	Description string `json:"description"`
	// Harness names the harness object (settings) an instance starts with;
	// "" means the daemon default. Account and Model are optional pins.
	Harness string `json:"harness,omitempty"`
	Account string `json:"account,omitempty"`
	Model   string `json:"model,omitempty"`

	Permissions Permissions `json:"permissions"`
	// Memory is "per-instance" (default) or "shared": whether an agent's own
	// notes are kept per project or across them (spec §7).
	Memory string `json:"memory,omitempty"`
	// Start is "fresh" (default) or "continue" (spec §8).
	Start           string `json:"start,omitempty"`
	IdleExitMinutes int    `json:"idleExitMinutes"`
	KeepAlive       bool   `json:"keepAlive"`

	// MaxTurnsPerTask, MaxTokensPerTask and SideEffects are recorded for the
	// lenses and the spec; the daemon does not enforce or render them yet
	// (docs/agent-spec.md §10). Memory and Start likewise: stored, not acted on.
	MaxTurnsPerTask  int      `json:"maxTurnsPerTask"`
	MaxTokensPerTask int      `json:"maxTokensPerTask"`
	SideEffects      []string `json:"sideEffects,omitempty"`
	AddDirs          []string `json:"addDirs,omitempty"`

	// Version comes from .claude-plugin/plugin.json; Save bumps it when the
	// generated content changed. Read-only through the API.
	Version string `json:"version,omitempty"`
	// Instructions is the base AGENTS.md body. Empty on Save keeps the file.
	Instructions string `json:"instructions,omitempty"`
}

// Defaults are the values a Definition gets for fields left zero (spec §5-10).
const (
	DefaultIdleExitMinutes  = 10
	DefaultMaxTurnsPerTask  = 40
	DefaultMaxTokensPerTask = 400_000
	DefaultHarness          = "opencode"
)

var namePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{2,23}$`)

// permissionValues is the closed vocabulary for the four gates.
var permissionValues = map[string]bool{"allow": true, "ask": true, "deny": true}

// Reject validates a definition before anything is written; "" accepts.
// Harness names are checked by the API (it holds the registry).
func Reject(d Definition) string {
	if !namePattern.MatchString(d.Name) {
		return fmt.Sprintf("agent name %q: use 3 to 24 chars of a-z, 0-9 and -, starting with a letter or digit", d.Name)
	}
	if strings.TrimSpace(d.Description) == "" {
		return fmt.Sprintf("agent %q needs a one-sentence description — other agents decide from it whether to call this one", d.Name)
	}
	if len(d.Description) > 160 {
		return fmt.Sprintf("agent %q: description over 160 chars", d.Name)
	}
	if msg := rejectPermissions(d.Name, d.Permissions); msg != "" {
		return msg
	}
	return rejectLifecycle(d)
}

// rejectLifecycle checks the closed vocabularies and the numeric caps.
func rejectLifecycle(d Definition) string {
	if d.Memory != "" && d.Memory != "per-instance" && d.Memory != "shared" {
		return fmt.Sprintf("agent %q: memory must be per-instance or shared", d.Name)
	}
	if d.Start != "" && d.Start != "fresh" && d.Start != "continue" {
		return fmt.Sprintf("agent %q: start must be fresh or continue", d.Name)
	}
	if d.IdleExitMinutes < 0 || d.MaxTurnsPerTask < 0 || d.MaxTokensPerTask < 0 {
		return fmt.Sprintf("agent %q: limits cannot be negative", d.Name)
	}
	return ""
}

func rejectPermissions(name string, p Permissions) string {
	for field, v := range map[string]string{"read": p.Read, "edit": p.Edit, "bash": p.Bash, "webfetch": p.Webfetch} {
		if v != "" && !permissionValues[v] {
			return fmt.Sprintf("agent %q: permission %s must be allow, ask or deny", name, field)
		}
	}
	return ""
}

// withDefaults fills the zero fields so every stored agent.json is complete
// and the lenses never guess (spec: defaults are written, not implied).
func withDefaults(d Definition) Definition {
	def := func(v *string, dflt string) {
		if *v == "" {
			*v = dflt
		}
	}
	def(&d.Harness, DefaultHarness)
	def(&d.Memory, "per-instance")
	def(&d.Start, "fresh")
	def(&d.Permissions.Read, "allow")
	def(&d.Permissions.Edit, "allow")
	def(&d.Permissions.Bash, "ask")
	def(&d.Permissions.Webfetch, "ask")
	// IdleExitMinutes is NOT defaulted here: 0 is a real value ("never put it
	// to sleep", decideAgent) and must survive a save. New agents get the
	// 10-minute default from Defaults(), which the API merges a request over.
	if d.MaxTurnsPerTask == 0 {
		d.MaxTurnsPerTask = DefaultMaxTurnsPerTask
	}
	if d.MaxTokensPerTask == 0 {
		d.MaxTokensPerTask = DefaultMaxTokensPerTask
	}
	return d
}

// Drifted is the ONE definition of "the base moved since this instance last
// started": both versions known and different. A blank pane version (a
// stamp that never persisted) is unknown, not drift — the lifecycle and the
// lenses must agree on that, so neither spells the rule itself.
func Drifted(paneVersion, baseVersion string) bool {
	return paneVersion != "" && baseVersion != "" && paneVersion != baseVersion
}

// Defaults is the definition a NEW agent starts from before the request is
// merged over it: the same fill as withDefaults plus the idle cap, which
// withDefaults leaves alone because 0 means "never sleep" once stored.
func Defaults() Definition {
	return withDefaults(Definition{IdleExitMinutes: DefaultIdleExitMinutes})
}

// ErrNotFound is returned for a name with no base folder.
var ErrNotFound = errors.New("agent not found")

// Store is the agents root folder, ~/.ccmux/agents by default.
type Store struct {
	Root string
	// now is stubbed by tests for the CHANGELOG stamp.
	now func() time.Time
}

// NewStore returns a Store over root, creating nothing until the first Save.
func NewStore(root string) *Store { return &Store{Root: root, now: time.Now} }

// Dir is the base folder of one agent.
func (s *Store) Dir(name string) string { return filepath.Join(s.Root, name) }

// List reads every base folder. An unreadable folder is an error, never a
// silently short list; a root that does not exist yet is an empty list.
func (s *Store) List() ([]Definition, error) {
	entries, err := os.ReadDir(s.Root)
	if errors.Is(err, os.ErrNotExist) {
		return []Definition{}, nil
	}
	if err != nil {
		return nil, err
	}
	out := []Definition{}
	for _, e := range entries {
		if !e.IsDir() || strings.HasPrefix(e.Name(), ".") {
			continue
		}
		d, err := s.Get(e.Name())
		if err != nil {
			return nil, fmt.Errorf("agent %q: %w", e.Name(), err)
		}
		out = append(out, d)
	}
	return out, nil
}

// ValidName reports whether name may be used as a folder segment. Every
// entry that turns a name into a path checks it: a name is user input from
// a URL or a bus message, and "../x" must never reach the filesystem.
func ValidName(name string) bool { return namePattern.MatchString(name) }

// Get reads one base: agent.json, the manifest version and AGENTS.md.
func (s *Store) Get(name string) (Definition, error) {
	if !ValidName(name) {
		return Definition{}, ErrNotFound
	}
	raw, err := os.ReadFile(filepath.Join(s.Dir(name), "agent.json"))
	if errors.Is(err, os.ErrNotExist) {
		return Definition{}, ErrNotFound
	}
	if err != nil {
		return Definition{}, err
	}
	var d Definition
	if err := json.Unmarshal(raw, &d); err != nil {
		return Definition{}, fmt.Errorf("agent.json: %w", err)
	}
	d.Name = name // the folder is the identity
	d.Version, err = readVersion(s.Dir(name))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return Definition{}, err // a corrupt manifest must not read as "version unknown, drift everywhere"
	}
	body, err := os.ReadFile(filepath.Join(s.Dir(name), "AGENTS.md"))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return Definition{}, err
	}
	d.Instructions = string(body)
	return withDefaults(d), nil
}
