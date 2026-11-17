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
// claudeDir, its loose skills, every instance home under the windows
// folder and, when one ran, a probe session (capprobe.go), which adds
// claude.ai's synced plugins and connectors.
func (s *Store) ImportCapabilities(claudeDir string, probe *ProbeResult) (ImportResult, error) {
	homes, err := s.readHomes()
	if err != nil {
		return ImportResult{}, err
	}
	cands, skipped, err := gatherCandidates(claudeDir, homes, probe)
	if err != nil {
		return ImportResult{}, err
	}
	capMu.Lock()
	defer capMu.Unlock()
	catalog, err := s.Capabilities()
	if err != nil {
		return ImportResult{}, err
	}
	sources, err := s.Sources()
	if err != nil {
		return ImportResult{}, err
	}
	same := sameSourcePlugin(sources)
	res := ImportResult{Added: []string{}, Skipped: orEmpty(skipped), Proposals: map[string]Proposal{}}
	for _, c := range cands {
		catalog = addCandidate(catalog, c, &res, same)
	}
	if err := s.writeCapabilities(catalog); err != nil {
		return ImportResult{}, err
	}
	for name, hs := range homes {
		res.Proposals[name] = propose(hs, catalog)
	}
	return res, nil
}

// gatherCandidates is every entry the import would add, sorted by id, and
// what a probe could not import.
func gatherCandidates(claudeDir string, homes map[string][]home, probe *ProbeResult) ([]Capability, []string, error) {
	installed, err := ReadInstalledPlugins(InstalledPluginsPath(claudeDir))
	if err != nil {
		return nil, nil, err
	}
	loose, err := LooseSkills(claudeDir)
	if err != nil {
		return nil, nil, err
	}
	var tools toolCounts
	var probed []Capability
	var skipped []string
	if probe != nil {
		tools = toolCounts(orEmpty(probe.Tools))
		probed, skipped = probeCandidates(*probe)
	}
	out := slices.Concat(importCandidates(installed, homes, tools), probed, loose)
	sort.SliceStable(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, skipped, nil
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
func importCandidates(installed map[string]string, homes map[string][]home, tools toolCounts) []Capability {
	var out []Capability
	for id, dir := range installed {
		name, market, _ := strings.Cut(id, "@")
		out = append(out, Capability{ID: name, Kind: CapPlugin, Plugin: id, Source: market, Version: manifestVersion(dir), Cost: pluginCost(dir, tools.plugin(name))})
	}
	for _, hs := range homes {
		for _, h := range hs {
			for name, e := range h.servers {
				out = append(out, Capability{ID: name, Kind: CapMCP, Server: &e, Cost: serverCost(-1)})
			}
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// sameSourcePlugin says whether candidate c, an installed plugin, is the
// plugin that source entry e holds: e's id at the marketplace e's source
// says it is (qa@personal is the qa of a source whose marketplace.json
// calls itself "personal", whatever the source's own id).
func sameSourcePlugin(sources []CapSource) func(e, c Capability) bool {
	market := map[string]string{}
	for _, src := range sources {
		market[src.ID] = src.Marketplace
	}
	return func(e, c Capability) bool {
		m := market[e.Source]
		return fromCheckout(e) && m != "" && c.Plugin == e.ID+"@"+m
	}
}

// addCandidate adds c unless the catalog has it: the same plugin or an
// identical server is already there (nothing to say), or its id is taken
// by something else (Skipped). A candidate that does not validate is
// skipped too.
func addCandidate(catalog []Capability, c Capability, res *ImportResult, same func(e, c Capability) bool) []Capability {
	c, err := checkCapability(c)
	if err != nil {
		res.Skipped = append(res.Skipped, c.ID+": "+err.Error())
		return catalog
	}
	for _, e := range catalog {
		if e.ID == c.ID {
			// The same plugin, kept by ccmux's own checkout of its
			// source, is there already.
			if !same(e, c) && !sameCapability(e, c) && !slices.Contains(res.Skipped, c.ID) {
				res.Skipped = append(res.Skipped, c.ID)
			}
			return catalog
		}
	}
	res.Added = append(res.Added, c.ID)
	return append(catalog, c)
}

// sameCapability says whether a and b are the same thing to load: the
// same kind and target. Description, source, version, cost and pin are
// what the daemon learns about it, not what it is.
func sameCapability(a, b Capability) bool {
	return capIdentity(a) == capIdentity(b)
}

func capIdentity(c Capability) string {
	j, _ := json.Marshal(Capability{Kind: c.Kind, Plugin: c.Plugin, Path: c.Path, Server: c.Server, Connector: c.Connector})
	return string(j)
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
	switch {
	case c.Kind == CapMCP:
		return enabled["mcp:"+c.ID]
	case c.Kind != CapPlugin:
		return false
	case enabled[c.Plugin] && c.Plugin != "":
		return true
	}
	name, _, _ := strings.Cut(c.Plugin, "@")
	if c.Plugin == "" {
		// A plugin folder (a source's checkout): a home enabling the plugin
		// of that name from any marketplace wants it.
		name = c.ID
		for id, on := range enabled {
			if on && strings.HasPrefix(id, name+"@") {
				return true
			}
		}
	}
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
