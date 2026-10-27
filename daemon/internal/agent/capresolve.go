package agent

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// CapabilityLaunch is what one isolated start loads from the catalog: the
// plugin folders in the order the base names them, and the MCP servers by
// name.
type CapabilityLaunch struct {
	PluginDirs []string
	MCP        map[string]mcpEntry
}

// ResolveCapabilities turns the ids a base names into what its start
// loads. An id missing from the catalog, or an installed plugin no longer
// installed, fails the start: an agent that wakes quietly without a tool
// it was given does its job wrong instead of not at all.
func ResolveCapabilities(ids []string, catalog []Capability, installed map[string]string) (CapabilityLaunch, error) {
	byID := map[string]Capability{}
	for _, c := range catalog {
		byID[c.ID] = c
	}
	out := CapabilityLaunch{MCP: map[string]mcpEntry{}}
	for _, id := range ids {
		c, ok := byID[id]
		if !ok {
			return out, fmt.Errorf("capability %q is not in the catalog", id)
		}
		if c.Kind == CapMCP {
			if c.Server == nil { // only a hand-edited catalog gets here
				return out, fmt.Errorf("capability %q is an MCP entry with no server", id)
			}
			out.MCP[c.ID] = *c.Server
			continue
		}
		dir, err := pluginDir(c, installed)
		if err != nil {
			return out, err
		}
		out.PluginDirs = append(out.PluginDirs, dir)
	}
	return out, nil
}

func pluginDir(c Capability, installed map[string]string) (string, error) {
	if c.Path != "" {
		return c.Path, nil
	}
	dir, ok := installed[c.Plugin]
	if !ok {
		return "", fmt.Errorf("capability %q: plugin %s is not installed on this host", c.ID, c.Plugin)
	}
	return dir, nil
}

// InstalledPluginsPath is Claude Code's record of installed plugins.
func InstalledPluginsPath(claudeDir string) string {
	return filepath.Join(claudeDir, "plugins", "installed_plugins.json")
}

// ReadInstalledPlugins maps each installed plugin id onto its install
// folder. A plugin installed in several scopes keeps the most recently
// updated install. No file is no plugins.
func ReadInstalledPlugins(path string) (map[string]string, error) {
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return map[string]string{}, nil
	}
	if err != nil {
		return nil, err
	}
	var doc struct {
		Plugins map[string][]struct {
			InstallPath string `json:"installPath"`
			LastUpdated string `json:"lastUpdated"`
		} `json:"plugins"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	out := map[string]string{}
	for id, installs := range doc.Plugins {
		newest := ""
		for _, in := range installs {
			if in.InstallPath != "" && in.LastUpdated >= newest {
				newest, out[id] = in.LastUpdated, in.InstallPath
			}
		}
	}
	return out, nil
}

// PluginSkills lists the skills a plugin folder brings, qualified the way
// the SDK's skills option names them: <plugin name>:<skill folder>.
func PluginSkills(dir string) ([]string, error) {
	name, err := pluginName(dir)
	if err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(filepath.Join(dir, skillsFolder))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []string
	for _, e := range entries {
		if e.IsDir() && !strings.HasPrefix(e.Name(), ".") {
			if _, err := os.Stat(filepath.Join(dir, skillsFolder, e.Name(), skillFile)); err == nil {
				out = append(out, name+":"+e.Name())
			}
		}
	}
	sort.Strings(out)
	return out, nil
}

func pluginName(dir string) (string, error) {
	raw, err := os.ReadFile(filepath.Join(dir, fileManifest))
	if err != nil {
		return "", fmt.Errorf("plugin %s: %w", dir, err)
	}
	var m struct {
		Name string `json:"name"`
	}
	if err := json.Unmarshal(raw, &m); err != nil || m.Name == "" {
		return "", fmt.Errorf("plugin %s: %s names no plugin", dir, fileManifest)
	}
	return m.Name, nil
}

// EnabledSkills is the skills option for a start loading dirs (the base
// first): nil when off is empty, so every skill stays on; otherwise every
// skill the folders bring except those switched off. The CLI's own built-in
// skills are then off too, since the option lists the only skills on.
func EnabledSkills(dirs, off []string) ([]string, error) {
	if len(off) == 0 {
		return nil, nil
	}
	skip := map[string]bool{}
	for _, s := range off {
		skip[s] = true
	}
	enabled := []string{}
	for _, dir := range dirs {
		names, err := PluginSkills(dir)
		if err != nil {
			return nil, err
		}
		for _, n := range names {
			if !skip[n] {
				enabled = append(enabled, n)
			}
		}
	}
	return enabled, nil
}

// capsMCPDir holds the generated MCP file of each isolated base's
// capabilities, written at every start (WriteCapabilityMCP).
const capsMCPDir = ".ccmux/caps"

// WriteCapabilityMCP writes the capability servers of base name in the
// neutral mcp.json shape the sidecar reads (--mcp-file) and returns its
// path. Under the agents root, not the instance home, so the agent cannot
// edit what its next start loads.
func (s *Store) WriteCapabilityMCP(name string, servers map[string]mcpEntry) (string, error) {
	if !ValidName(name) {
		return "", ErrNotFound
	}
	path := filepath.Join(s.Root, capsMCPDir, name+".mcp.json")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return "", err
	}
	if servers == nil {
		servers = map[string]mcpEntry{}
	}
	blob, _ := json.MarshalIndent(servers, "", "  ")
	_, err := writeIfChanged(path, append(blob, '\n'))
	return path, err
}

// IsolatedLaunchFor is what an isolated start of base d loads: its
// capabilities resolved against the catalog and the plugins installed
// under claudeDir, their servers written to the generated MCP file, the
// peers shim's command, and the enabled skills of the base and its plugins.
func (s *Store) IsolatedLaunchFor(d Definition, claudeDir, peersCommand string) (*IsolatedLaunch, error) {
	catalog, err := s.Capabilities()
	if err != nil {
		return nil, err
	}
	installed, err := ReadInstalledPlugins(InstalledPluginsPath(claudeDir))
	if err != nil {
		return nil, err
	}
	caps, err := ResolveCapabilities(d.Capabilities, catalog, installed)
	if err != nil {
		return nil, err
	}
	mcpFile, err := s.WriteCapabilityMCP(d.Name, caps.MCP)
	if err != nil {
		return nil, err
	}
	skills, err := EnabledSkills(append([]string{s.Dir(d.Name)}, caps.PluginDirs...), d.SkillsOff)
	if err != nil {
		return nil, err
	}
	return &IsolatedLaunch{PluginDirs: caps.PluginDirs, MCPFile: mcpFile, PeersCommand: peersCommand, Skills: skills}, nil
}
