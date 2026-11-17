package agent

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
)

// ReadHostSetup reads what a session started in folders (its own folder and
// its repo's) would load by itself from this host's Claude setup under
// claudeDir. It is read at every start, from the files Claude Code itself
// reads, so a plugin installed yesterday is switched off today without
// anyone telling the catalog.
//
// The installed plugins are the one file that must read: without it nothing
// is known and the session would load everything. A settings file that
// does not parse is said in notes and left out, since Claude Code will not
// read it either.
func ReadHostSetup(claudeDir string, folders ...string) (HostSetup, []string, error) {
	installed, err := ReadInstalledPlugins(InstalledPluginsPath(claudeDir))
	if err != nil {
		return HostSetup{}, nil, err
	}
	host := HostSetup{Installed: installed, Synced: syncedPlugins(claudeDir), SkillsDir: filepath.Join(claudeDir, skillsFolder)}
	var notes []string
	settings := []string{filepath.Join(claudeDir, "settings.json")}
	for _, dir := range uniqueSorted(folders) {
		settings = append(settings, filepath.Join(dir, ".claude", "settings.json"), filepath.Join(dir, ".claude", "settings.local.json"))
		host.RepoSkills = append(host.RepoSkills, skillFolders(filepath.Join(dir, ".claude", skillsFolder))...)
	}
	for _, path := range settings {
		ids, err := enabledPluginIDs(path)
		if err != nil {
			notes = append(notes, "not read, so plugins only it names stay as Claude Code loads them: "+err.Error())
		}
		host.PluginIDs = append(host.PluginIDs, ids...)
	}
	host.PluginIDs = uniqueSorted(host.PluginIDs)
	host.RepoSkills = uniqueSorted(host.RepoSkills)
	servers, err := userServers(claudeDir, folders)
	if err != nil {
		notes = append(notes, "not read, so the user's own MCP servers stay on: "+err.Error())
	}
	host.Servers = servers
	return host, notes, nil
}

// syncedPlugins are the plugins claude.ai syncs to this machine, name →
// folder: plugins/synced/<account>/<plugin>/, each with a manifest.
func syncedPlugins(claudeDir string) map[string]string {
	out := map[string]string{}
	manifests, _ := filepath.Glob(filepath.Join(claudeDir, "plugins", "synced", "*", "*", fileManifest))
	for _, m := range manifests {
		dir := filepath.Dir(filepath.Dir(m))
		if name, err := pluginName(dir); err == nil {
			out[name] = dir
		}
	}
	return out
}

// enabledPluginIDs are the plugin ids a settings file names, on or off:
// either way Claude Code knows the id, and so must the off list.
func enabledPluginIDs(path string) ([]string, error) {
	var doc struct {
		EnabledPlugins map[string]bool `json:"enabledPlugins"`
	}
	if err := readJSONIfPresent(path, &doc); err != nil {
		return nil, err
	}
	ids := make([]string, 0, len(doc.EnabledPlugins))
	for id := range doc.EnabledPlugins {
		ids = append(ids, id)
	}
	return ids, nil
}

// EnabledPlugins are the plugin ids a repo's own settings switch on
// (.claude/settings.json and .claude/settings.local.json): what its
// sessions use today, which the day-one choices carry over. A file that
// does not parse is returned as an error beside what the other file says:
// Claude Code does not load plugins from a file it cannot read either.
func EnabledPlugins(repo string) ([]string, error) {
	var out []string
	var errs []error
	for _, name := range []string{"settings.json", "settings.local.json"} {
		var doc struct {
			EnabledPlugins map[string]bool `json:"enabledPlugins"`
		}
		if err := readJSONIfPresent(filepath.Join(repo, ".claude", name), &doc); err != nil {
			errs = append(errs, err)
			continue
		}
		for id, on := range doc.EnabledPlugins {
			if on {
				out = append(out, id)
			}
		}
	}
	return uniqueSorted(out), errors.Join(errs...)
}

// CatalogIDOfPlugin is the catalog entry that holds installed plugin id
// (name@marketplace): the entry naming that id, or the entry a source's
// checkout keeps for the same plugin (qa@personal is the qa of a source
// whose marketplace calls itself "personal").
func CatalogIDOfPlugin(catalog []Capability, sources []CapSource, id string) (string, bool) {
	same := sameSourcePlugin(sources)
	for _, c := range catalog {
		if c.Kind != CapPlugin {
			continue
		}
		if c.Plugin == id || same(c, Capability{Plugin: id}) {
			return c.ID, true
		}
	}
	return "", false
}

// skillFolders are the skill folders directly under root, by name.
func skillFolders(root string) []string {
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil
	}
	var out []string
	for _, e := range entries {
		if !e.IsDir() || strings.HasPrefix(e.Name(), ".") {
			continue
		}
		if _, err := os.Stat(filepath.Join(root, e.Name(), skillFile)); err == nil {
			out = append(out, e.Name())
		}
	}
	return out
}

// userServers are the MCP servers the user's own Claude config names: the
// ones for every folder, and the ones for these folders. Claude Code keeps
// them in .claude.json, beside the config folder (~/.claude.json) or, with
// CLAUDE_CONFIG_DIR set, inside it.
func userServers(claudeDir string, folders []string) ([]string, error) {
	var doc struct {
		Servers  map[string]any `json:"mcpServers"`
		Projects map[string]struct {
			Servers map[string]any `json:"mcpServers"`
		} `json:"projects"`
	}
	path := filepath.Join(claudeDir, ".claude.json")
	if _, err := os.Stat(path); err != nil {
		path = filepath.Join(filepath.Dir(claudeDir), ".claude.json")
	}
	if err := readJSONIfPresent(path, &doc); err != nil {
		return nil, err
	}
	var out []string
	for name := range doc.Servers {
		out = append(out, name)
	}
	for _, dir := range folders {
		for name := range doc.Projects[dir].Servers {
			out = append(out, name)
		}
	}
	return uniqueSorted(out), nil
}
