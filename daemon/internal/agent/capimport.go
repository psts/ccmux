package agent

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
)

// The import seeds the catalog from what agents use today through the host
// user's Claude Code setup: every installed plugin, and every MCP server in
// an instance home's .mcp.json. It only reads those files, and it never
// replaces a catalog entry. Per base it also proposes what an isolated
// start would need to keep working (Proposal), which it does not apply:
// a human checks it against the agent's brief first.

// Proposal is one base's suggested isolated setup, read from its homes'
// .claude/settings.local.json and .mcp.json.
type Proposal struct {
	Capabilities []string `json:"capabilities"`
	ToolAllow    []string `json:"toolAllow"`
	ToolDeny     []string `json:"toolDeny"`
}

// ImportResult is what an import added and what it proposes per base.
// Skipped names a home server whose id is taken by a different entry.
type ImportResult struct {
	Added     []string            `json:"added"`
	Skipped   []string            `json:"skipped"`
	Proposals map[string]Proposal `json:"proposals"`
}

// homeSettings is the part of a home's settings.local.json the import reads.
type homeSettings struct {
	Permissions struct {
		Allow []string `json:"allow"`
		Deny  []string `json:"deny"`
	} `json:"permissions"`
	EnabledPlugins             map[string]bool `json:"enabledPlugins"`
	EnabledMcpjsonServers      []string        `json:"enabledMcpjsonServers"`
	EnableAllProjectMcpServers bool            `json:"enableAllProjectMcpServers"`
}

// home is one instance folder as the import reads it.
type home struct {
	settings homeSettings
	servers  map[string]mcpEntry
}

// ImportCapabilities runs the import against the plugins installed under
// claudeDir and every instance home under the windows folder.
func (s *Store) ImportCapabilities(claudeDir string) (ImportResult, error) {
	installed, err := ReadInstalledPlugins(InstalledPluginsPath(claudeDir))
	if err != nil {
		return ImportResult{}, err
	}
	homes, err := s.readHomes()
	if err != nil {
		return ImportResult{}, err
	}
	capMu.Lock()
	defer capMu.Unlock()
	catalog, err := s.Capabilities()
	if err != nil {
		return ImportResult{}, err
	}
	res := ImportResult{Added: []string{}, Skipped: []string{}, Proposals: map[string]Proposal{}}
	for _, c := range importCandidates(installed, homes) {
		catalog = addCandidate(catalog, c, &res)
	}
	if err := s.writeCapabilities(catalog); err != nil {
		return ImportResult{}, err
	}
	for name, hs := range homes {
		res.Proposals[name] = propose(hs, catalog)
	}
	return res, nil
}

// readHomes reads every instance home, by base name, under every window.
func (s *Store) readHomes() (map[string][]home, error) {
	dirs, err := filepath.Glob(filepath.Join(filepath.Dir(s.Root), "windows", "*", "agents", "*"))
	if err != nil {
		return nil, err
	}
	out := map[string][]home{}
	for _, dir := range dirs {
		name := filepath.Base(dir)
		if !ValidName(name) {
			continue
		}
		h, err := readHome(dir)
		if err != nil {
			return nil, err
		}
		out[name] = append(out[name], h)
	}
	return out, nil
}

func readHome(dir string) (home, error) {
	var h home
	if err := readJSONIfPresent(filepath.Join(dir, ".claude", "settings.local.json"), &h.settings); err != nil {
		return h, err
	}
	var doc struct {
		Servers map[string]mcpEntry `json:"mcpServers"`
	}
	if err := readJSONIfPresent(filepath.Join(dir, ".mcp.json"), &doc); err != nil {
		return h, err
	}
	h.servers = doc.Servers
	return h, nil
}

func readJSONIfPresent(path string, v any) error {
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if err := json.Unmarshal(raw, v); err != nil {
		return errors.New(path + ": " + err.Error())
	}
	return nil
}

// importCandidates are the entries the import would add: one per installed
// plugin (id = the plugin's name) and one per home MCP server (id = its
// name), sorted by id.
func importCandidates(installed map[string]string, homes map[string][]home) []Capability {
	var out []Capability
	for id := range installed {
		name, _, _ := strings.Cut(id, "@")
		out = append(out, Capability{ID: name, Kind: CapPlugin, Plugin: id})
	}
	for _, hs := range homes {
		for _, h := range hs {
			for name, e := range h.servers {
				out = append(out, Capability{ID: name, Kind: CapMCP, Server: &e})
			}
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// addCandidate adds c unless the catalog has it: the same plugin or an
// identical server is already there (nothing to say), or its id is taken
// by something else (Skipped). A candidate that does not validate is
// skipped too.
func addCandidate(catalog []Capability, c Capability, res *ImportResult) []Capability {
	c, err := checkCapability(c)
	if err != nil {
		res.Skipped = append(res.Skipped, c.ID+": "+err.Error())
		return catalog
	}
	for _, e := range catalog {
		if e.ID == c.ID {
			if !sameCapability(e, c) && !slices.Contains(res.Skipped, c.ID) {
				res.Skipped = append(res.Skipped, c.ID)
			}
			return catalog
		}
	}
	res.Added = append(res.Added, c.ID)
	return append(catalog, c)
}

func sameCapability(a, b Capability) bool {
	ja, _ := json.Marshal(a)
	jb, _ := json.Marshal(b)
	return string(ja) == string(jb)
}

// propose reads one base's homes: every plugin capability whose tools a
// rule names or that a home enables, every home server a home switches
// on, and the homes' allow and deny rules as they are, deduplicated.
func propose(hs []home, catalog []Capability) Proposal {
	var rules, allow, deny, caps []string
	enabled := map[string]bool{}
	for _, h := range hs {
		allow = append(allow, h.settings.Permissions.Allow...)
		deny = append(deny, h.settings.Permissions.Deny...)
		for id, on := range h.settings.EnabledPlugins {
			enabled[id] = enabled[id] || on
		}
		for _, name := range h.settings.EnabledMcpjsonServers {
			enabled["mcp:"+name] = true
		}
		for name := range h.servers {
			enabled["mcp:"+name] = enabled["mcp:"+name] || h.settings.EnableAllProjectMcpServers
		}
	}
	rules = append(slices.Clone(allow), deny...)
	for _, c := range catalog {
		if wanted(c, enabled, rules) {
			caps = append(caps, c.ID)
		}
	}
	return Proposal{Capabilities: orEmpty(caps), ToolAllow: dedupe(allow), ToolDeny: dedupe(deny)}
}

// wanted says whether a home uses capability c: enabled outright, or (a
// plugin) named by a rule on its tools, mcp__plugin_<name>_<server>.
func wanted(c Capability, enabled map[string]bool, rules []string) bool {
	if c.Kind == CapMCP {
		return enabled["mcp:"+c.ID]
	}
	if enabled[c.Plugin] {
		return true
	}
	name, _, _ := strings.Cut(c.Plugin, "@")
	return slices.ContainsFunc(rules, func(r string) bool { return strings.HasPrefix(r, "mcp__plugin_"+name+"_") })
}

func dedupe(list []string) []string {
	out := slices.Clone(list)
	sort.Strings(out)
	return orEmpty(slices.Compact(out))
}

func orEmpty(list []string) []string {
	if list == nil {
		return []string{}
	}
	return list
}
