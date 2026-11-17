package agent

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"sort"
)

// A loadout is what one thread session (a runner session: a repo's lead, a
// thread, a thread's helper) loads from the catalog on top of its fixed
// base. The session reads the user's own settings (CLAUDE.md and memory
// come that way), so "load only this" is said as four switches the Agent
// SDK honours, each tested on SDK 0.3.311 with Claude Code 2.1.326:
//
//   - every plugin id the host knows is set on or off in the session's own
//     settings, which win over the user's and the repo's settings files;
//   - a plugin that is a folder (a source's checkout, a pin) loads by path;
//   - the skills on are listed, and a skill not listed is not offered;
//   - the MCP servers not chosen are denied by name, and claude.ai's
//     connectors are switched off whole when none is chosen.
//
// The bus (PeersServer) is the fixed base and never part of this: nothing
// here can deny or replace it.

// PeersServer is the bus's MCP server, as Claude Code names it.
const PeersServer = "claude-peers"

// Loadout is the file a session's runner reads (claudeserve/loadout.mjs).
// Every list is there even when empty: a missing skills list would mean
// "every skill", the opposite of an empty one.
type Loadout struct {
	// PluginDirs are the plugin folders loaded by path.
	PluginDirs []string `json:"pluginDirs"`
	// EnabledPlugins switches every plugin id the host knows on or off.
	EnabledPlugins map[string]bool `json:"enabledPlugins"`
	// Skills are the only skills offered to the model. One typed as /name
	// still runs.
	Skills []string `json:"skills"`
	// DeniedServers are MCP servers and connectors refused by name.
	DeniedServers []string `json:"deniedServers"`
	// Connectors false switches every claude.ai connector off, known to the
	// catalog or not.
	Connectors bool `json:"connectors"`
	// MCP are the chosen catalog servers, in the neutral mcp.json shape.
	MCP map[string]mcpEntry `json:"mcp"`
}

// LoadoutEntry is one catalog entry a session loaded.
type LoadoutEntry struct {
	ID      string `json:"id"`
	Kind    string `json:"kind"`
	Version string `json:"version,omitempty"`
	Commit  string `json:"commit,omitempty"`
}

// LoadoutRecord is what a session's row keeps of the loadout it started
// with: what loaded, what was skipped and why (Notes), and the signature a
// later look compares against (LoadoutSignature).
type LoadoutRecord struct {
	Signature string         `json:"signature"`
	Entries   []LoadoutEntry `json:"entries"`
	Notes     []string       `json:"notes"`
	StartedAt int64          `json:"startedAt,omitempty"`
}

// HostSetup is what this host's Claude setup holds at a session's start
// (ReadHostSetup): everything a session would load by itself, which is
// what has to be switched off.
type HostSetup struct {
	// Installed is every installed plugin id with its folder; Synced every
	// plugin claude.ai syncs, by name, which a session knows as name@synced.
	Installed map[string]string
	Synced    map[string]string
	// PluginIDs are ids a settings file names that are in neither.
	PluginIDs []string
	// Servers are the user's own MCP servers, by name.
	Servers []string
	// SkillsDir is where a session loads loose skills from.
	SkillsDir string
	// RepoSkills are the repo's own skills, which stay on: they are the
	// repo's, like its CLAUDE.md, and the catalog cannot switch them.
	RepoSkills []string
}

// loadoutBuilder gathers one loadout.
type loadoutBuilder struct {
	host      HostSetup
	out       Loadout
	rec       LoadoutRecord
	connector map[string]bool // chosen connectors, by Claude Code's name
}

// ResolveLoadout turns the catalog ids a session's group and repo leave on
// into what it loads. An id that cannot be loaded (gone from the catalog,
// not installed here, a folder that is not there) is skipped and said in
// the record's notes; it never fails the start.
func ResolveLoadout(catalog []Capability, ids []string, host HostSetup) (Loadout, LoadoutRecord) {
	b := &loadoutBuilder{host: host, connector: map[string]bool{}}
	b.out = Loadout{PluginDirs: []string{}, EnabledPlugins: knownPluginsOff(catalog, host), Skills: slices.Clone(host.RepoSkills), MCP: map[string]mcpEntry{}}
	b.rec = LoadoutRecord{Signature: LoadoutSignature(catalog, ids), Entries: []LoadoutEntry{}, Notes: []string{}}
	byID := map[string]Capability{}
	for _, c := range catalog {
		byID[c.ID] = c
	}
	for _, id := range uniqueSorted(ids) {
		c, ok := byID[id]
		if !ok {
			b.rec.Notes = append(b.rec.Notes, id+": no longer in the catalog, skipped")
			continue
		}
		if why := b.add(c); why != "" {
			b.rec.Notes = append(b.rec.Notes, id+": "+why+", skipped")
			continue
		}
		b.rec.Entries = append(b.rec.Entries, LoadoutEntry{ID: c.ID, Kind: c.Kind, Version: c.Version, Commit: c.Commit})
	}
	b.out.Skills = uniqueSorted(b.out.Skills)
	b.out.DeniedServers = b.denied(catalog)
	return b.out, b.rec
}

// knownPluginsOff is every plugin id the host or the catalog knows, off. A
// chosen entry switches its own id back on (addPlugin). Claude Code's own
// built-in plugins (cc-plugin-*@builtin) are not in it: they are part of
// Claude Code, not of the user's setup.
func knownPluginsOff(catalog []Capability, host HostSetup) map[string]bool {
	off := map[string]bool{}
	for id := range host.Installed {
		off[id] = false
	}
	for name := range host.Synced {
		off[name+syncedMarket] = false
	}
	for _, id := range host.PluginIDs {
		off[id] = false
	}
	for _, c := range catalog {
		if c.Kind == CapPlugin && c.Plugin != "" {
			off[c.Plugin] = false
		}
	}
	return off
}

// syncedMarket is what follows a claude.ai synced plugin's name in its id.
const syncedMarket = "@synced"

// add loads one entry, or says why it cannot be loaded.
func (b *loadoutBuilder) add(c Capability) string {
	switch c.Kind {
	case CapPlugin:
		return b.addPlugin(c)
	case CapSkill:
		return b.addSkill(c)
	case CapMCP:
		return b.addServer(c)
	case CapConnector:
		b.out.Connectors = true
		b.connector[c.Connector] = true
		return ""
	}
	return "a " + c.Kind + " is not something a session loads"
}

// addPlugin loads a plugin the way the host already knows it when it does
// (an installed plugin and a synced one stay on by their id, so the session
// runs the copy a terminal runs), and by its folder otherwise (a source's
// checkout, a pin), where the installed plugin of the same name stays off.
func (b *loadoutBuilder) addPlugin(c Capability) string {
	dir := c.Path
	if c.Plugin != "" {
		installed, ok := b.host.Installed[c.Plugin]
		if !ok {
			return "plugin " + c.Plugin + " is not installed on this host"
		}
		dir = installed
	}
	name, err := pluginName(dir)
	if err != nil {
		return "its folder holds no plugin (" + dir + ")"
	}
	skills, err := PluginSkills(dir)
	if err != nil {
		return "its skills could not be read: " + err.Error()
	}
	switch {
	case c.Plugin != "":
		b.out.EnabledPlugins[c.Plugin] = true
	case b.host.Synced[name] == dir:
		b.out.EnabledPlugins[name+syncedMarket] = true
	default:
		b.out.PluginDirs = append(b.out.PluginDirs, dir)
	}
	b.out.Skills = append(b.out.Skills, skills...)
	return ""
}

// addSkill lists a loose skill. A session loads loose skills from one
// folder only, so an entry pointing anywhere else cannot be loaded.
func (b *loadoutBuilder) addSkill(c Capability) string {
	if filepath.Dir(c.Path) != b.host.SkillsDir {
		return "its folder is not in " + b.host.SkillsDir + ", where a session loads skills from"
	}
	if _, err := os.Stat(filepath.Join(c.Path, skillFile)); err != nil {
		return "its folder holds no " + skillFile
	}
	b.out.Skills = append(b.out.Skills, filepath.Base(c.Path))
	return ""
}

func (b *loadoutBuilder) addServer(c Capability) string {
	if c.ID == PeersServer {
		return PeersServer + " is the bus, which every session has"
	}
	if c.Server == nil { // only a hand-edited catalog gets here
		return "it is an MCP entry with no server"
	}
	b.out.MCP[c.ID] = *c.Server
	return ""
}

// denied names every server a session would load by itself and was not
// given: the catalog's connectors that are not chosen, and the user's own
// servers. Never the bus.
func (b *loadoutBuilder) denied(catalog []Capability) []string {
	out := []string{}
	for _, c := range catalog {
		if c.Kind == CapConnector && !b.connector[c.Connector] {
			out = append(out, c.Connector)
		}
	}
	for _, name := range b.host.Servers {
		if _, chosen := b.out.MCP[name]; !chosen {
			out = append(out, name)
		}
	}
	out = uniqueSorted(out)
	return slices.DeleteFunc(out, func(name string) bool { return name == PeersServer })
}

// LoadoutSignature names what the ids load from this catalog: it changes
// when an id comes or goes, when a chosen entry's version, commit or
// target changes, and when one is deleted from the catalog. A session whose
// recorded signature is not the one computed now is out of date. It reads
// no disk, so the session list can ask it for every row.
func LoadoutSignature(catalog []Capability, ids []string) string {
	byID := map[string]Capability{}
	for _, c := range catalog {
		byID[c.ID] = c
	}
	type line struct {
		ID      string `json:"id"`
		Missing bool   `json:"missing,omitempty"`
		Version string `json:"version,omitempty"`
		Commit  string `json:"commit,omitempty"`
		Target  string `json:"target,omitempty"`
	}
	lines := []line{}
	for _, id := range uniqueSorted(ids) {
		c, ok := byID[id]
		lines = append(lines, line{ID: id, Missing: !ok, Version: c.Version, Commit: c.Commit, Target: capIdentity(c)})
	}
	blob, _ := json.Marshal(lines)
	sum := sha256.Sum256(blob)
	return hex.EncodeToString(sum[:8])
}

// uniqueSorted is list sorted with repeats dropped, never nil.
func uniqueSorted(list []string) []string {
	out := slices.Clone(list)
	sort.Strings(out)
	out = slices.Compact(out)
	if out == nil {
		return []string{}
	}
	return out
}

// LoadoutFor resolves ids against this host's catalog and Claude setup, for
// a session working in folders (its own folder and its repo's).
func (s *Store) LoadoutFor(ids []string, claudeDir string, folders ...string) (Loadout, LoadoutRecord, error) {
	catalog, err := s.Capabilities()
	if err != nil {
		return Loadout{}, LoadoutRecord{}, err
	}
	host, notes, err := ReadHostSetup(claudeDir, folders...)
	if err != nil {
		return Loadout{}, LoadoutRecord{}, err
	}
	lo, rec := ResolveLoadout(catalog, ids, host)
	rec.Notes = append(rec.Notes, notes...)
	return lo, rec, nil
}
