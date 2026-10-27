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

// Capabilities are what an isolated claude agent (Definition.Isolated)
// loads besides its own base: a Claude Code plugin (an installed one by its
// id, or a plugin folder by path) or one MCP server in the neutral mcp.json
// shape. One catalog per host, under the agents root; a base names the
// entries it wants (Definition.Capabilities). Secrets belong in the
// instance's .env, which fills an entry's ${VAR} references at start
// (claudeserve/mcp.mjs), the same as a base's own mcp.json. Only headers,
// where tokens usually go, are refused a literal value (literalHeader); env
// values and urls are served to the lenses as written.

// Capability kinds.
const (
	CapPlugin = "plugin"
	CapMCP    = "mcp"
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
	Server      *mcpEntry `json:"server,omitempty"`
	SideEffects []string  `json:"sideEffects,omitempty"`
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
		list[i] = c
	} else {
		list = append(list, c)
	}
	return c, s.writeCapabilities(list)
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
	users, err := s.capabilityUsers(id)
	if err != nil {
		return err
	}
	if len(users) > 0 {
		return fmt.Errorf("%w: %s is still on %v", ErrCapabilityInUse, id, users)
	}
	return s.writeCapabilities(slices.Delete(list, i, i+1))
}

func (s *Store) capabilityUsers(id string) ([]string, error) {
	defs, err := s.List()
	if err != nil {
		return nil, err
	}
	var users []string
	for _, d := range defs {
		if slices.Contains(d.Capabilities, id) {
			users = append(users, d.Name)
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
	switch c.Kind {
	case CapPlugin:
		if (c.Plugin == "") == (c.Path == "") {
			return c, fmt.Errorf("capability %s: a plugin names an installed plugin id or a folder path, exactly one", c.ID)
		}
		if c.Path != "" && !filepath.IsAbs(c.Path) {
			return c, fmt.Errorf("capability %s: the plugin path must be absolute", c.ID)
		}
		c.Server = nil
	case CapMCP:
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
		c.Server, c.Plugin, c.Path = &e, "", ""
	default:
		return c, fmt.Errorf("capability %s: kind must be plugin or mcp", c.ID)
	}
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
