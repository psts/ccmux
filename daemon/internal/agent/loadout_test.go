package agent

import (
	"encoding/json"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// loadoutRig is a catalog of every kind over real folders, and the host
// setup a session would find beside it.
type loadoutRig struct {
	catalog []Capability
	host    HostSetup
	qaDir   string // an installed plugin's folder (qa@personal)
	kbDir   string // a source checkout's folder
	pinDir  string // a pinned entry's worktree folder
	synced  string // a claude.ai synced plugin's folder
}

func pluginFolder(t *testing.T, dir, name string, skills ...string) string {
	t.Helper()
	writeFile(t, filepath.Join(dir, fileManifest), `{"name":"`+name+`","version":"1.0"}`)
	for _, sk := range skills {
		writeFile(t, filepath.Join(dir, skillsFolder, sk, skillFile), "---\nname: "+sk+"\n---\n")
	}
	return dir
}

func newLoadoutRig(t *testing.T) loadoutRig {
	t.Helper()
	root := t.TempDir()
	r := loadoutRig{
		qaDir:  pluginFolder(t, filepath.Join(root, "cache", "qa"), "qa", "check", "review"),
		kbDir:  pluginFolder(t, filepath.Join(root, "ckout", "kb"), "kb", "write"),
		pinDir: pluginFolder(t, filepath.Join(root, "pins", "abc", "gate"), "gate", "stop"),
		synced: pluginFolder(t, filepath.Join(root, "synced", "acct", "marketing"), "marketing", "seo"),
	}
	skills := filepath.Join(root, "claude", "skills")
	writeFile(t, filepath.Join(skills, "glitchtip", skillFile), "x")
	r.catalog = []Capability{
		{ID: "qa", Kind: CapPlugin, Plugin: "qa@personal", Version: "0.8.0"},
		{ID: "kb", Kind: CapPlugin, Path: r.kbDir, Source: "personal", Version: "1.6.0", Commit: "c1"},
		{ID: "gate", Kind: CapPlugin, Path: r.pinDir, Source: "personal", Version: "1.9.0", Commit: "abc", Pinned: true},
		{ID: "claude.ai-marketing", Kind: CapPlugin, Path: r.synced, Source: SourceClaudeAI},
		{ID: "glitchtip", Kind: CapSkill, Path: filepath.Join(skills, "glitchtip"), Source: SourceThisMachine},
		{ID: "claude.ai-Gmail", Kind: CapConnector, Connector: "claude.ai Gmail"},
		{ID: "claude.ai-Drive", Kind: CapConnector, Connector: "claude.ai Google Drive"},
		{ID: "search", Kind: CapMCP, Server: &mcpEntry{URL: "https://s/mcp"}},
	}
	r.host = HostSetup{
		Installed:  map[string]string{"qa@personal": r.qaDir, "stripe@official": filepath.Join(root, "cache", "stripe")},
		Synced:     map[string]string{"marketing": r.synced, "saac-soul": filepath.Join(root, "synced", "acct", "saac-soul")},
		PluginIDs:  []string{"kb@personal", "music@personal"},
		Servers:    []string{"claude_design", PeersServer, "search"},
		SkillsDir:  skills,
		RepoSkills: []string{"deploy"},
	}
	return r
}

// TestResolveLoadout is the table of what a session loads for the ids its
// group and repo leave on.
func TestResolveLoadout(t *testing.T) {
	r := newLoadoutRig(t)
	allOff := map[string]bool{"qa@personal": false, "stripe@official": false, "kb@personal": false, "music@personal": false, "marketing@synced": false, "saac-soul@synced": false}
	with := func(on ...string) map[string]bool {
		m := map[string]bool{}
		for k, v := range allOff {
			m[k] = v
		}
		for _, id := range on {
			m[id] = true
		}
		return m
	}
	bothConnectors := []string{"claude.ai Gmail", "claude.ai Google Drive", "claude_design", "search"}
	cases := []struct {
		name    string
		ids     []string
		dirs    []string
		enabled map[string]bool
		skills  []string
		denied  []string
		conn    bool
		mcp     []string
		entries []string
		note    string
	}{
		{name: "nothing chosen", ids: nil, dirs: []string{}, enabled: allOff, skills: []string{"deploy"}, denied: bothConnectors, entries: []string{}},
		{name: "an installed plugin stays on by its id", ids: []string{"qa"}, dirs: []string{}, enabled: with("qa@personal"),
			skills: []string{"deploy", "qa:check", "qa:review"}, denied: bothConnectors, entries: []string{"qa"}},
		{name: "a checkout plugin loads by folder, its installed id off", ids: []string{"kb"}, dirs: []string{r.kbDir}, enabled: allOff,
			skills: []string{"deploy", "kb:write"}, denied: bothConnectors, entries: []string{"kb"}},
		{name: "a pinned entry loads from its pin's folder", ids: []string{"gate"}, dirs: []string{r.pinDir}, enabled: allOff,
			skills: []string{"deploy", "gate:stop"}, denied: bothConnectors, entries: []string{"gate"}},
		{name: "a synced plugin stays on by its synced id", ids: []string{"claude.ai-marketing"}, dirs: []string{}, enabled: with("marketing@synced"),
			skills: []string{"deploy", "marketing:seo"}, denied: bothConnectors, entries: []string{"claude.ai-marketing"}},
		{name: "a loose skill joins the list", ids: []string{"glitchtip"}, dirs: []string{}, enabled: allOff,
			skills: []string{"deploy", "glitchtip"}, denied: bothConnectors, entries: []string{"glitchtip"}},
		{name: "a chosen connector keeps connectors on and the others denied", ids: []string{"claude.ai-Gmail"}, dirs: []string{}, enabled: allOff,
			skills: []string{"deploy"}, denied: []string{"claude.ai Google Drive", "claude_design", "search"}, conn: true, entries: []string{"claude.ai-Gmail"}},
		{name: "a chosen server is passed and not denied", ids: []string{"search"}, dirs: []string{}, enabled: allOff,
			skills: []string{"deploy"}, denied: []string{"claude.ai Gmail", "claude.ai Google Drive", "claude_design"}, mcp: []string{"search"}, entries: []string{"search"}},
		{name: "an entry deleted from the catalog is skipped and said", ids: []string{"qa", "gone"}, dirs: []string{}, enabled: with("qa@personal"),
			skills: []string{"deploy", "qa:check", "qa:review"}, denied: bothConnectors, entries: []string{"qa"}, note: "gone: no longer in the catalog"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			lo, rec := ResolveLoadout(r.catalog, c.ids, r.host)
			if !slices.Equal(lo.PluginDirs, c.dirs) || lo.PluginDirs == nil {
				t.Errorf("plugin folders = %v, want %v", lo.PluginDirs, c.dirs)
			}
			if !mapsEqual(lo.EnabledPlugins, c.enabled) {
				t.Errorf("enabledPlugins = %v, want %v", lo.EnabledPlugins, c.enabled)
			}
			if !slices.Equal(lo.Skills, c.skills) {
				t.Errorf("skills = %v, want %v", lo.Skills, c.skills)
			}
			if !slices.Equal(lo.DeniedServers, c.denied) {
				t.Errorf("denied = %v, want %v", lo.DeniedServers, c.denied)
			}
			if lo.Connectors != c.conn {
				t.Errorf("connectors = %v, want %v", lo.Connectors, c.conn)
			}
			if got := mapKeys(lo.MCP); !slices.Equal(got, c.mcp) {
				t.Errorf("mcp = %v, want %v", got, c.mcp)
			}
			ids := []string{}
			for _, e := range rec.Entries {
				ids = append(ids, e.ID)
			}
			if !slices.Equal(ids, c.entries) {
				t.Errorf("entries = %v, want %v", ids, c.entries)
			}
			if (c.note == "") != (len(rec.Notes) == 0) || (c.note != "" && !strings.Contains(rec.Notes[0], c.note)) {
				t.Errorf("notes = %v, want one holding %q", rec.Notes, c.note)
			}
		})
	}
}

func mapsEqual(a, b map[string]bool) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if w, ok := b[k]; !ok || v != w {
			return false
		}
	}
	return true
}

func mapKeys[V any](m map[string]V) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	slices.Sort(out)
	return out
}

// An empty loadout must say "nothing" in so many words: a list the sidecar
// reads as missing means "everything", which is the opposite.
func TestResolveLoadout_EmptyListsAreListsInJSON(t *testing.T) {
	lo, rec := ResolveLoadout(nil, nil, HostSetup{})
	blob, _ := json.Marshal(lo)
	for _, want := range []string{`"pluginDirs":[]`, `"skills":[]`, `"deniedServers":[]`, `"enabledPlugins":{}`, `"mcp":{}`, `"connectors":false`} {
		if !strings.Contains(string(blob), want) {
			t.Errorf("loadout JSON %s lacks %s", blob, want)
		}
	}
	blob, _ = json.Marshal(rec)
	for _, want := range []string{`"entries":[]`, `"notes":[]`} {
		if !strings.Contains(string(blob), want) {
			t.Errorf("record JSON %s lacks %s", blob, want)
		}
	}
}

// The bus is the fixed base: nothing a catalog or a user's config says may
// deny it or replace it.
func TestResolveLoadout_NeverTouchesTheBus(t *testing.T) {
	catalog := []Capability{
		{ID: PeersServer, Kind: CapMCP, Server: &mcpEntry{Command: "evil"}},
		{ID: "bus", Kind: CapConnector, Connector: PeersServer},
	}
	lo, rec := ResolveLoadout(catalog, []string{PeersServer}, HostSetup{Servers: []string{PeersServer}})
	if slices.Contains(lo.DeniedServers, PeersServer) {
		t.Errorf("the bus is denied: %v", lo.DeniedServers)
	}
	if _, ok := lo.MCP[PeersServer]; ok {
		t.Errorf("a catalog entry replaced the bus: %v", lo.MCP)
	}
	if len(rec.Notes) != 1 || !strings.Contains(rec.Notes[0], "bus") {
		t.Errorf("the skipped entry is not said: %v", rec.Notes)
	}
}

func TestResolveLoadout_SkipsWhatTheHostLacks(t *testing.T) {
	r := newLoadoutRig(t)
	catalog := append(slices.Clone(r.catalog),
		Capability{ID: "ghost", Kind: CapPlugin, Plugin: "ghost@nowhere"},
		Capability{ID: "empty", Kind: CapPlugin, Path: filepath.Join(t.TempDir(), "nothing")},
		Capability{ID: "far", Kind: CapSkill, Path: filepath.Join(t.TempDir(), "far")},
	)
	lo, rec := ResolveLoadout(catalog, []string{"ghost", "empty", "far", "qa"}, r.host)
	if len(rec.Entries) != 1 || rec.Entries[0].ID != "qa" || len(rec.Notes) != 3 {
		t.Fatalf("entries %v notes %v: want qa loaded and three skipped", rec.Entries, rec.Notes)
	}
	if lo.EnabledPlugins["ghost@nowhere"] || len(lo.PluginDirs) != 0 {
		t.Errorf("a skipped entry still loads: %+v", lo)
	}
}

// The signature is what "out of date" compares: it moves with the chosen
// ids and with a chosen entry's version, and with nothing else.
func TestLoadoutSignature(t *testing.T) {
	r := newLoadoutRig(t)
	base := LoadoutSignature(r.catalog, []string{"qa", "kb"})
	if base != LoadoutSignature(r.catalog, []string{"kb", "qa", "qa"}) {
		t.Error("order and repeats of the ids change the signature")
	}
	if base == LoadoutSignature(r.catalog, []string{"qa"}) {
		t.Error("a dropped id does not change the signature")
	}
	moved := slices.Clone(r.catalog)
	moved[1].Version, moved[1].Commit = "1.7.0", "c2"
	if base == LoadoutSignature(moved, []string{"qa", "kb"}) {
		t.Error("a chosen entry's new version does not change the signature")
	}
	other := slices.Clone(r.catalog)
	other[2].Version = "2.0.0"
	if base != LoadoutSignature(other, []string{"qa", "kb"}) {
		t.Error("an entry that is not chosen changes the signature")
	}
	if base == LoadoutSignature(r.catalog[1:], []string{"qa", "kb"}) {
		t.Error("a chosen entry deleted from the catalog does not change the signature")
	}
	_, rec := ResolveLoadout(r.catalog, []string{"qa", "kb"}, r.host)
	if rec.Signature != base {
		t.Error("the record's signature is not the one a later check computes")
	}
}

func TestReadHostSetup(t *testing.T) {
	root := t.TempDir()
	claude := filepath.Join(root, ".claude")
	repo := filepath.Join(root, "repo")
	writeFile(t, InstalledPluginsPath(claude), `{"plugins":{"qa@personal":[{"installPath":"/c/qa","lastUpdated":"2"}]}}`)
	pluginFolder(t, filepath.Join(claude, "plugins", "synced", "acct_org", "marketing"), "marketing")
	writeFile(t, filepath.Join(claude, "plugins", "synced", "acct_org", "manifest.json"), `{}`)
	writeFile(t, filepath.Join(claude, "settings.json"), `{"enabledPlugins":{"music@personal":true,"old@gone":false}}`)
	writeFile(t, filepath.Join(repo, ".claude", "settings.local.json"), `{"enabledPlugins":{"kb@personal":true}}`)
	writeFile(t, filepath.Join(repo, ".claude", "settings.json"), `not json`)
	writeFile(t, filepath.Join(repo, ".claude", skillsFolder, "deploy", skillFile), "x")
	writeFile(t, filepath.Join(repo, ".claude", skillsFolder, "notes.txt"), "x")
	writeFile(t, filepath.Join(root, ".claude.json"), `{"mcpServers":{"claude_design":{},"claude-peers":{}},"projects":{"`+repo+`":{"mcpServers":{"hidpi":{}}},"/other":{"mcpServers":{"nope":{}}}}}`)

	host, notes, err := ReadHostSetup(claude, repo, repo)
	if err != nil {
		t.Fatal(err)
	}
	if host.Installed["qa@personal"] != "/c/qa" || host.Synced["marketing"] == "" || len(host.Synced) != 1 {
		t.Errorf("installed %v synced %v", host.Installed, host.Synced)
	}
	if want := []string{"kb@personal", "music@personal", "old@gone"}; !slices.Equal(host.PluginIDs, want) {
		t.Errorf("plugin ids = %v, want %v", host.PluginIDs, want)
	}
	if want := []string{"claude-peers", "claude_design", "hidpi"}; !slices.Equal(host.Servers, want) {
		t.Errorf("servers = %v, want %v", host.Servers, want)
	}
	if !slices.Equal(host.RepoSkills, []string{"deploy"}) || host.SkillsDir != filepath.Join(claude, skillsFolder) {
		t.Errorf("repo skills %v, skills dir %s", host.RepoSkills, host.SkillsDir)
	}
	if len(notes) != 1 || !strings.Contains(notes[0], "settings.json") {
		t.Errorf("an unreadable settings file is said once, not fatal: %v", notes)
	}
}

func TestLoadoutFor_ReadsCatalogAndHost(t *testing.T) {
	s := testStore(t)
	claude := filepath.Join(t.TempDir(), ".claude")
	dir := pluginFolder(t, filepath.Join(t.TempDir(), "qa"), "qa", "check")
	writeFile(t, InstalledPluginsPath(claude), `{"plugins":{"qa@personal":[{"installPath":"`+dir+`","lastUpdated":"2"}]}}`)
	if _, err := s.SaveCapability(Capability{ID: "qa", Kind: CapPlugin, Plugin: "qa@personal"}); err != nil {
		t.Fatal(err)
	}
	lo, rec, err := s.LoadoutFor([]string{"qa", "gone"}, claude)
	if err != nil {
		t.Fatal(err)
	}
	if !lo.EnabledPlugins["qa@personal"] || !slices.Equal(lo.Skills, []string{"qa:check"}) || len(rec.Notes) != 1 {
		t.Errorf("loadout %+v record %+v", lo, rec)
	}
}
