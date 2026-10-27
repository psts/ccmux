package agent

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
)

func writeFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestCapabilityCatalog(t *testing.T) {
	s := testStore(t)
	if list, err := s.Capabilities(); err != nil || len(list) != 0 {
		t.Fatalf("no catalog yet is an empty list: %v %v", list, err)
	}
	door := Capability{ID: "x-door", Kind: CapMCP, Server: &mcpEntry{URL: "http://127.0.0.1:8930/x/mcp", Headers: map[string]string{"Authorization": "${X_DOOR_TOKEN}"}}}
	saved, err := s.SaveCapability(door)
	if err != nil || saved.Server.Type != "http" {
		t.Fatalf("a url server gets the http type: %+v %v", saved, err)
	}
	if _, err := s.SaveCapability(Capability{ID: "kb", Kind: CapPlugin, Plugin: "chartlabs-kb@personal", Server: &mcpEntry{Command: "x"}}); err != nil {
		t.Fatal(err)
	}
	list, _ := s.Capabilities()
	if len(list) != 2 || list[0].ID != "kb" || list[0].Server != nil {
		t.Fatalf("sorted by id, a plugin keeps no server: %+v", list)
	}
	for _, bad := range []Capability{
		{ID: "../x", Kind: CapMCP, Server: &mcpEntry{Command: "c"}},
		{ID: "p", Kind: CapPlugin},
		{ID: "p", Kind: CapPlugin, Plugin: "a@b", Path: "/p"},
		{ID: "p", Kind: CapPlugin, Path: "relative"},
		{ID: "m", Kind: CapMCP},
		{ID: "m", Kind: "skill"},
	} {
		if _, err := s.SaveCapability(bad); err == nil {
			t.Errorf("accepted %+v", bad)
		}
	}
	if _, err := s.Save(Definition{Name: "radar", Description: "d", Capabilities: []string{"kb"}}); err != nil {
		t.Fatal(err)
	}
	if err := s.RemoveCapability("kb"); !errors.Is(err, ErrCapabilityInUse) || !strings.Contains(err.Error(), "radar") {
		t.Errorf("an entry a base names stays, and says who: %v", err)
	}
	if err := s.RemoveCapability("x-door"); err != nil {
		t.Fatal(err)
	}
	if err := s.RemoveCapability("x-door"); !errors.Is(err, ErrNotFound) {
		t.Errorf("gone is not found: %v", err)
	}
}

func TestRejectCapabilityFields(t *testing.T) {
	for _, d := range []Definition{
		{Name: "radar", Capabilities: []string{"a/b"}},
		{Name: "radar", ToolAllow: []string{"Bash(git log --format=%h,%s)"}},
		{Name: "radar", ToolDeny: []string{" "}},
		{Name: "radar", SkillsOff: []string{"a,b"}},
	} {
		if rejectCapabilityFields(d) == "" {
			t.Errorf("accepted %+v", d)
		}
	}
	if msg := rejectCapabilityFields(Definition{Name: "radar", Capabilities: []string{"x-door-alex_chartlabs"}, ToolAllow: []string{"mcp__kb"}}); msg != "" {
		t.Error(msg)
	}
}

func TestResolveCapabilities(t *testing.T) {
	catalog := []Capability{
		{ID: "kb", Kind: CapPlugin, Plugin: "chartlabs-kb@personal"},
		{ID: "local", Kind: CapPlugin, Path: "/plugins/local"},
		{ID: "door", Kind: CapMCP, Server: &mcpEntry{Type: "http", URL: "http://d/mcp"}},
	}
	installed := map[string]string{"chartlabs-kb@personal": "/cache/kb/1.5.3"}
	got, err := ResolveCapabilities([]string{"local", "door", "kb"}, catalog, installed)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got.PluginDirs, []string{"/plugins/local", "/cache/kb/1.5.3"}) || got.MCP["door"].URL != "http://d/mcp" || len(got.MCP) != 1 {
		t.Errorf("%+v", got)
	}
	if _, err := ResolveCapabilities([]string{"bare"}, append(catalog, Capability{ID: "bare", Kind: CapMCP}), installed); err == nil {
		t.Error("a hand-edited MCP entry without a server must fail the start, not crash it")
	}
	if _, err := ResolveCapabilities([]string{"gone"}, catalog, installed); err == nil {
		t.Error("an id missing from the catalog must fail the start")
	}
	if _, err := ResolveCapabilities([]string{"kb"}, catalog, map[string]string{}); err == nil || !strings.Contains(err.Error(), "not installed") {
		t.Errorf("an uninstalled plugin must fail the start: %v", err)
	}
}

func TestReadInstalledPluginsKeepsNewest(t *testing.T) {
	path := filepath.Join(t.TempDir(), "installed_plugins.json")
	writeFile(t, path, `{"version":2,"plugins":{"kb@p":[
		{"installPath":"/c/kb/1.5.2","lastUpdated":"2026-09-01T00:00:00Z"},
		{"installPath":"/c/kb/1.5.3","lastUpdated":"2026-10-08T00:00:00Z"},
		{"installPath":"/c/kb/1.5.1","lastUpdated":"2026-08-01T00:00:00Z"}]}}`)
	got, err := ReadInstalledPlugins(path)
	if err != nil || got["kb@p"] != "/c/kb/1.5.3" {
		t.Errorf("%v %v", got, err)
	}
	if got, err := ReadInstalledPlugins(filepath.Join(t.TempDir(), "none.json")); err != nil || len(got) != 0 {
		t.Errorf("no file is no plugins: %v %v", got, err)
	}
}

func TestEnabledSkills(t *testing.T) {
	root := t.TempDir()
	base, kb := filepath.Join(root, "radar"), filepath.Join(root, "kb")
	writeFile(t, filepath.Join(base, fileManifest), `{"name":"radar"}`)
	writeFile(t, filepath.Join(base, "skills", "scan", skillFile), "x")
	writeFile(t, filepath.Join(base, "skills", "notes", "README.md"), "no SKILL.md, not a skill")
	writeFile(t, filepath.Join(kb, fileManifest), `{"name":"chartlabs-kb"}`)
	writeFile(t, filepath.Join(kb, "skills", "draft", skillFile), "x")
	writeFile(t, filepath.Join(kb, "skills", "publish", skillFile), "x")
	if got, err := EnabledSkills([]string{base, kb}, nil); err != nil || got != nil {
		t.Errorf("nothing off is no list (every skill on): %v %v", got, err)
	}
	got, err := EnabledSkills([]string{base, kb}, []string{"chartlabs-kb:publish"})
	if err != nil || !reflect.DeepEqual(got, []string{"radar:scan", "chartlabs-kb:draft"}) {
		t.Errorf("%v %v", got, err)
	}
	if _, err := EnabledSkills([]string{filepath.Join(root, "nomanifest")}, []string{"x"}); err == nil {
		t.Error("a plugin folder without a manifest must fail")
	}
}

func TestIsolatedLaunchFor(t *testing.T) {
	s := testStore(t)
	claude := t.TempDir()
	writeFile(t, InstalledPluginsPath(claude), `{"plugins":{"kb@p":[{"installPath":"/c/kb","lastUpdated":"x"}]}}`)
	if _, err := s.SaveCapability(Capability{ID: "kb", Kind: CapPlugin, Plugin: "kb@p"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SaveCapability(Capability{ID: "door", Kind: CapMCP, Server: &mcpEntry{URL: "http://d/mcp"}}); err != nil {
		t.Fatal(err)
	}
	d, err := s.Save(Definition{Name: "radar", Description: "d", Isolated: true, Capabilities: []string{"kb", "door"}})
	if err != nil {
		t.Fatal(err)
	}
	in, err := s.IsolatedLaunchFor(d, claude, "/bin/ccmux-peers")
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(in.PluginDirs, []string{"/c/kb"}) || in.PeersCommand != "/bin/ccmux-peers" || in.Skills != nil {
		t.Errorf("%+v", in)
	}
	body, _ := os.ReadFile(in.MCPFile)
	if !strings.HasPrefix(in.MCPFile, s.Root) || !strings.Contains(string(body), `"url": "http://d/mcp"`) {
		t.Errorf("the capability servers land under the agents root in the neutral shape: %s %s", in.MCPFile, body)
	}
}

func TestImportCapabilities(t *testing.T) {
	s := testStore(t)
	claude := t.TempDir()
	writeFile(t, InstalledPluginsPath(claude), `{"plugins":{
		"chartlabs-kb@personal":[{"installPath":"/c/kb","lastUpdated":"x"}],
		"chrome-pool@personal":[{"installPath":"/c/cp","lastUpdated":"x"}],
		"music@personal":[{"installPath":"/c/m","lastUpdated":"x"}]}}`)
	windows := filepath.Join(filepath.Dir(s.Root), "windows")
	radar := filepath.Join(windows, "w1", "agents", "radar")
	writeFile(t, filepath.Join(radar, ".claude", "settings.local.json"), `{"permissions":{
		"allow":["mcp__plugin_chartlabs-kb_kb-api__kb_get_doc","mcp__claude-peers","Write"],
		"deny":["mcp__plugin_chrome-pool_chrome-pool__browser_run_code"]}}`)
	watch := filepath.Join(windows, "w1", "agents", "x-watch")
	homeSettings := `{"permissions":{"allow":["mcp__x-door-a"]},"enabledMcpjsonServers":["x-door-a"]}`
	writeFile(t, filepath.Join(watch, ".claude", "settings.local.json"), homeSettings)
	doors := `{"mcpServers":{"x-door-a":{"type":"http","url":"http://127.0.0.1:8930/x/a/mcp","headers":{"Authorization":"Bearer ${X_A}"}},"x-door-b":{"type":"http","url":"http://127.0.0.1:8930/x/b/mcp"}}}`
	writeFile(t, filepath.Join(watch, ".mcp.json"), doors)
	// A second window's home of the same base: its rules join the proposal.
	writeFile(t, filepath.Join(windows, "w2", "agents", "radar", ".claude", "settings.local.json"), `{"permissions":{"allow":["Write","WebFetch"]}}`)
	if _, err := s.SaveCapability(Capability{ID: "music", Kind: CapPlugin, Path: "/elsewhere"}); err != nil {
		t.Fatal(err)
	}

	res, err := s.ImportCapabilities(claude)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(res.Added, []string{"chartlabs-kb", "chrome-pool", "x-door-a", "x-door-b"}) || !reflect.DeepEqual(res.Skipped, []string{"music"}) {
		t.Errorf("added %v skipped %v", res.Added, res.Skipped)
	}
	want := map[string]Proposal{
		"radar":   {Capabilities: []string{"chartlabs-kb", "chrome-pool"}, ToolAllow: []string{"WebFetch", "Write", "mcp__claude-peers", "mcp__plugin_chartlabs-kb_kb-api__kb_get_doc"}, ToolDeny: []string{"mcp__plugin_chrome-pool_chrome-pool__browser_run_code"}},
		"x-watch": {Capabilities: []string{"x-door-a"}, ToolAllow: []string{"mcp__x-door-a"}, ToolDeny: []string{}},
	}
	if !reflect.DeepEqual(res.Proposals, want) {
		t.Errorf("proposals\n got %+v\nwant %+v", res.Proposals, want)
	}
	list, _ := s.Capabilities()
	if music := list[slices.IndexFunc(list, func(c Capability) bool { return c.ID == "music" })]; music.Path != "/elsewhere" {
		t.Errorf("an existing entry is never replaced: %+v", music)
	}
	// Read only: the homes are as they were.
	if b, _ := os.ReadFile(filepath.Join(watch, ".mcp.json")); string(b) != doors {
		t.Error("the import wrote into a home")
	}
	if again, err := s.ImportCapabilities(claude); err != nil || len(again.Added) != 0 {
		t.Errorf("a second import adds nothing: %+v %v", again, err)
	}
}

func TestCapabilityRefusesLiteralHeader(t *testing.T) {
	s := testStore(t)
	_, err := s.SaveCapability(Capability{ID: "door", Kind: CapMCP, Server: &mcpEntry{URL: "http://d/mcp", Headers: map[string]string{"Authorization": "Bearer sk-live"}}})
	if err == nil || !strings.Contains(err.Error(), "Authorization") {
		t.Errorf("a literal header must be refused, naming it: %v", err)
	}
}
