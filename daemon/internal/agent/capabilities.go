package agent

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"sync"
)

// The capability catalog is the one list of everything a session can be
// given: a Claude Code plugin (an installed one by its id, or a plugin
// folder by path), one MCP server in the neutral mcp.json shape, a loose
// skill, or a claude.ai connector, each with where it came from, its
// version and its cost. Entries come from the import (capimport.go), the
// git sources (capsources.go) or a lens. One catalog per host, under the
// agents root. An isolated claude agent (Definition.Isolated) names the
// plugin and MCP entries it loads besides its own base
// (Definition.Capabilities). Secrets belong in the
// instance's .env, which fills an entry's ${VAR} references at start
// (claudeserve/mcp.mjs), the same as a base's own mcp.json. Only headers,
// where tokens usually go, are refused a literal value (literalHeader); env
// values and urls are served to the lenses as written.

// Capability kinds. A skill is one loose skill folder (today the folders
// in ~/.claude/skills, never its synced/ folder, which is claude.ai's); a
// connector is a claude.ai connector, which Claude Code loads by itself
// and the catalog only names. Isolated agents load plugins and MCP
// servers only (ResolveCapabilities); thread sessions use all four.
const (
	CapPlugin    = "plugin"
	CapMCP       = "mcp"
	CapSkill     = "skill"
	CapConnector = "connector"
)

// Origins that are not a git source: what claude.ai syncs (connectors,
// synced plugins) and what lives only in this machine's ~/.claude.
const (
	SourceClaudeAI    = "claude.ai"
	SourceThisMachine = "this machine"
)

// Capability is one catalog entry.
type Capability struct {
	ID          string `json:"id"`
	Kind        string `json:"kind"`
	Description string `json:"description,omitempty"`
	// Plugin is an installed plugin's id (name@marketplace), resolved to its
	// install folder at every start so a plugin update reaches the agent.
	// Path is a plugin folder instead. A plugin entry sets exactly one.
	Plugin string `json:"plugin,omitempty"`
	Path   string `json:"path,omitempty"`
	// Server is an MCP entry's server, named by the entry's id.
	Server *mcpEntry `json:"server,omitempty"`
	// Connector is a connector entry's name exactly as Claude Code names
	// it ("claude.ai Gmail"); the id is a slug of it.
	Connector   string   `json:"connector,omitempty"`
	SideEffects []string `json:"sideEffects,omitempty"`
	// Source is where the entry came from: a git source's id
	// (capsources.go), a marketplace an installed plugin came from,
	// SourceClaudeAI or SourceThisMachine. Version is the plugin manifest's
	// version, else the source's last commit on the plugin's folder.
	// Commit is the source commit the entry's folder is at. Cost is a
	// rough token estimate of what the entry adds to every session
	// (capcost.go). The daemon sets all four; a PUT keeps them.
	Source  string `json:"source,omitempty"`
	Version string `json:"version,omitempty"`
	Commit  string `json:"commit,omitempty"`
	Cost    int    `json:"cost,omitempty"`
	// Pinned keeps a source entry at Commit across source updates, in its
	// own folder (PinCapability). Set through the pin route only.
	Pinned bool `json:"pinned,omitempty"`
}

// capabilitiesFile is the catalog under the agents root, dot-prefixed so
// List skips its folder.
const capabilitiesFile = ".ccmux/capabilities.json"

var capabilityID = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,63}$`)

// ErrCapabilityInUse refuses removing an entry a base still names.
var ErrCapabilityInUse = errors.New("capability in use")

// Capabilities reads the catalog, sorted by id; none yet is an empty list.
func (s *Store) Capabilities() ([]Capability, error) {
	raw, err := os.ReadFile(filepath.Join(s.Root, capabilitiesFile))
	if errors.Is(err, os.ErrNotExist) {
		return []Capability{}, nil
	}
	if err != nil {
		return nil, err
	}
	var doc struct {
		Capabilities []Capability `json:"capabilities"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil, fmt.Errorf("%s: %w", capabilitiesFile, err)
	}
	if doc.Capabilities == nil {
		doc.Capabilities = []Capability{}
	}
	sort.Slice(doc.Capabilities, func(i, j int) bool { return doc.Capabilities[i].ID < doc.Capabilities[j].ID })
	return doc.Capabilities, nil
}

// capMu serializes every read-change-write of the catalog, so two lenses
// saving at once cannot drop one another's entry.
var capMu sync.Mutex

// SaveCapability adds an entry or replaces the one with its id.
func (s *Store) SaveCapability(c Capability) (Capability, error) {
	c, err := checkCapability(c)
	if err != nil {
		return Capability{}, err
	}
	capMu.Lock()
	defer capMu.Unlock()
	list, err := s.Capabilities()
	if err != nil {
		return Capability{}, err
	}
	i := slices.IndexFunc(list, func(e Capability) bool { return e.ID == c.ID })
	if i >= 0 {
		c = keepDaemonFields(list[i], c)
		list[i] = c
	} else {
		c = keepDaemonFields(Capability{}, c)
		list = append(list, c)
	}
	return c, s.writeCapabilities(list)
}

// keepDaemonFields carries what only the daemon sets from the stored entry
// old (none for a new entry) onto a lens's replacement c: where it came from, its version and
// cost, its pin, and (for an entry from a git source) the folder that
// source keeps, so a PUT can neither forge nor drop them.
// An entry PUT as another kind is a new thing and keeps none of them.
func keepDaemonFields(old, c Capability) Capability {
	c.Source, c.Version, c.Commit, c.Cost, c.Pinned = "", "", "", 0, false
	if c.Kind != old.Kind {
		return c
	}
	c.Source, c.Version, c.Commit, c.Cost, c.Pinned = old.Source, old.Version, old.Commit, old.Cost, old.Pinned
	if fromCheckout(old) {
		c.Path, c.Plugin = old.Path, old.Plugin
	}
	return c
}

// RemoveCapability drops an entry no base names; one still named is
// ErrCapabilityInUse with the bases in the message, so a wake never finds
// a capability gone from under it.
func (s *Store) RemoveCapability(id string) error {
	capMu.Lock()
	defer capMu.Unlock()
	list, err := s.Capabilities()
	if err != nil {
		return err
	}
	i := slices.IndexFunc(list, func(e Capability) bool { return e.ID == id })
	if i < 0 {
		return ErrNotFound
	}
	if err := s.refuseInUse(id); err != nil {
		return err
	}
	return s.writeCapabilities(slices.Delete(list, i, i+1))
}

// refuseInUse is ErrCapabilityInUse naming the users of id, nil when none.
func (s *Store) refuseInUse(id string) error {
	users, err := s.CapabilityUsers(id)
	if err != nil {
		return err
	}
	if len(users) > 0 {
		return fmt.Errorf("%w: %s is still on %v", ErrCapabilityInUse, id, users)
	}
	return nil
}

// CapabilityUsers answers "who uses catalog entry id", as one label per
// user ("agent radar"). It is the one place that question is asked: removing
// an entry, removing a source and re-pointing an imported entry at a source
// all go through it. Today the users are the bases that name the id; the
// group and repo choices (skills and plugins task 2) are added here.
func (s *Store) CapabilityUsers(id string) ([]string, error) {
	defs, err := s.List()
	if err != nil {
		return nil, err
	}
	var users []string
	for _, d := range defs {
		if slices.Contains(d.Capabilities, id) {
			users = append(users, "agent "+d.Name)
		}
	}
	return users, nil
}

func (s *Store) writeCapabilities(list []Capability) error {
	path := filepath.Join(s.Root, capabilitiesFile)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	blob, _ := json.MarshalIndent(struct {
		Capabilities []Capability `json:"capabilities"`
	}{list}, "", "  ")
	_, err := writeIfChanged(path, append(blob, '\n'))
	return err
}

// rejectCapabilityFields checks a base's capability ids and tool rules. A
// rule travels in a comma-joined flag (claudePermissionFlags), so it may
// hold no comma; whether a named capability exists is the start's to say
// (ResolveCapabilities), since the catalog can change after a save.
func rejectCapabilityFields(d Definition) string {
	for _, id := range d.Capabilities {
		if !capabilityID.MatchString(id) {
			return fmt.Sprintf("agent %q: capability %q is not a catalog id", d.Name, id)
		}
	}
	for _, rule := range slices.Concat(d.ToolAllow, d.ToolDeny, d.SkillsOff) {
		if strings.TrimSpace(rule) == "" || strings.Contains(rule, ",") {
			return fmt.Sprintf("agent %q: rule %q must be non-empty and hold no comma", d.Name, rule)
		}
	}
	return ""
}

// checkCapability validates one entry and clears the fields its kind does
// not use, so a stored entry says one thing.
func checkCapability(c Capability) (Capability, error) {
	if !capabilityID.MatchString(c.ID) {
		return c, fmt.Errorf("capability id %q: use 1 to 64 chars of letters, digits, _ . and -", c.ID)
	}
	check, ok := kindCheckers[c.Kind]
	if !ok {
		return c, fmt.Errorf("capability %s: kind must be plugin, mcp, skill or connector", c.ID)
	}
	return check(c)
}

var kindCheckers = map[string]func(Capability) (Capability, error){
	CapPlugin:    checkPluginCap,
	CapMCP:       checkMCPCap,
	CapSkill:     checkSkillCap,
	CapConnector: checkConnectorCap,
}

func checkPluginCap(c Capability) (Capability, error) {
	if (c.Plugin == "") == (c.Path == "") {
		return c, fmt.Errorf("capability %s: a plugin names an installed plugin id or a folder path, exactly one", c.ID)
	}
	if c.Path != "" && !filepath.IsAbs(c.Path) {
		return c, fmt.Errorf("capability %s: the plugin path must be absolute", c.ID)
	}
	c.Server, c.Connector = nil, ""
	return c, nil
}

func checkMCPCap(c Capability) (Capability, error) {
	if c.Server == nil {
		return c, fmt.Errorf("capability %s: an mcp entry needs a server", c.ID)
	}
	m, err := checkMCP(map[string]mcpEntry{c.ID: *c.Server})
	if err != nil {
		return c, err
	}
	if h := literalHeader(m[c.ID]); h != "" {
		return c, fmt.Errorf("capability %s: header %s holds a literal value; the catalog holds no secrets, write ${VAR} and set it in the instance's .env", c.ID, h)
	}
	e := m[c.ID]
	c.Server, c.Plugin, c.Path, c.Connector = &e, "", "", ""
	return c, nil
}

func checkSkillCap(c Capability) (Capability, error) {
	if !filepath.IsAbs(c.Path) {
		return c, fmt.Errorf("capability %s: a skill names its folder by absolute path", c.ID)
	}
	c.Server, c.Plugin, c.Connector = nil, "", ""
	return c, nil
}

func checkConnectorCap(c Capability) (Capability, error) {
	if strings.TrimSpace(c.Connector) == "" || strings.ContainsAny(c.Connector, "\n\r\t") {
		return c, fmt.Errorf("capability %s: a connector names the claude.ai connector, as Claude Code names it", c.ID)
	}
	c.Server, c.Plugin, c.Path = nil, "", ""
	return c, nil
}

// literalHeader names a header whose value references no ${VAR}: a header
// is where a server's token goes, and the catalog is served to every lens.
func literalHeader(e mcpEntry) string {
	names := make([]string, 0, len(e.Headers))
	for name := range e.Headers {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		if !strings.Contains(e.Headers[name], "${") {
			return name
		}
	}
	return ""
}
