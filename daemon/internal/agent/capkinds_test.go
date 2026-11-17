package agent

import (
	"errors"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestCapabilityKinds_SkillAndConnector(t *testing.T) {
	s := testStore(t)
	sk, err := s.SaveCapability(Capability{ID: "glitchtip", Kind: CapSkill, Path: "/home/u/.claude/skills/glitchtip", Server: &mcpEntry{URL: "http://x"}})
	if err != nil || sk.Server != nil {
		t.Fatalf("a skill keeps its path and no server: %+v %v", sk, err)
	}
	con, err := s.SaveCapability(Capability{ID: "claude.ai-Gmail", Kind: CapConnector, Connector: "claude.ai Gmail", Path: "/x"})
	if err != nil || con.Path != "" || con.Connector != "claude.ai Gmail" {
		t.Fatalf("a connector keeps only its name: %+v %v", con, err)
	}
	for _, bad := range []Capability{
		{ID: "s", Kind: CapSkill, Path: "relative"},
		{ID: "c", Kind: CapConnector},
		{ID: "c", Kind: CapConnector, Connector: "two\nlines"},
		{ID: "k", Kind: "theme"},
	} {
		if _, err := s.SaveCapability(bad); err == nil {
			t.Errorf("%+v should be refused", bad)
		}
	}
	catalog, _ := s.Capabilities()
	for _, id := range []string{"glitchtip", "claude.ai-Gmail"} {
		if _, err := ResolveCapabilities([]string{id}, catalog, nil); err == nil || !strings.Contains(err.Error(), "plugins and MCP servers only") {
			t.Errorf("an isolated agent cannot load %s yet: %v", id, err)
		}
	}
}

func TestSaveCapability_KeepsDaemonFields(t *testing.T) {
	s := testStore(t)
	forged, _ := s.SaveCapability(Capability{ID: "kb", Kind: CapPlugin, Path: "/p/kb", Source: "personal", Version: "9", Commit: "abc", Cost: 5, Pinned: true})
	if forged.Source != "" || forged.Commit != "" || forged.Pinned || forged.Cost != 0 {
		t.Fatalf("a new entry gets no daemon fields from a lens: %+v", forged)
	}
	// What a source sync writes, then a lens PUT on top of it.
	capMu.Lock()
	s.writeCapabilities([]Capability{{ID: "kb", Kind: CapPlugin, Path: "/ckout/kb", Source: "personal", Version: "1.0", Commit: "abc", Cost: 40, Pinned: true}})
	capMu.Unlock()
	got, err := s.SaveCapability(Capability{ID: "kb", Kind: CapPlugin, Path: "/elsewhere", Description: "mine", SideEffects: []string{"posts"}})
	if err != nil || got.Path != "/ckout/kb" || got.Version != "1.0" || !got.Pinned || got.Cost != 40 || got.Description != "mine" {
		t.Fatalf("a PUT keeps the source's folder, version, pin and cost and takes the rest: %+v %v", got, err)
	}
	other, _ := s.SaveCapability(Capability{ID: "kb", Kind: CapMCP, Server: &mcpEntry{URL: "http://d/mcp"}})
	if other.Source != "" || other.Pinned {
		t.Fatalf("a PUT as another kind is a new entry: %+v", other)
	}
}

func TestCapabilityUsers(t *testing.T) {
	s := testStore(t)
	s.SaveCapability(Capability{ID: "door", Kind: CapMCP, Server: &mcpEntry{URL: "http://d/mcp"}})
	s.Save(Definition{Name: "radar", Description: "d", Harness: "claude", Capabilities: []string{"door"}})
	users, err := s.CapabilityUsers("door")
	if err != nil || !slices.Equal(users, []string{"agent radar"}) {
		t.Fatalf("users = %v %v", users, err)
	}
	if err := s.RemoveCapability("door"); !errors.Is(err, ErrCapabilityInUse) || !strings.Contains(err.Error(), "agent radar") {
		t.Fatalf("remove refuses naming the user: %v", err)
	}
}

func TestLooseSkillsAndCost(t *testing.T) {
	claude := t.TempDir()
	writeFile(t, filepath.Join(claude, "skills", "glitchtip", skillFile), "---\nname: glitchtip\ndescription: Read GlitchTip issues\n---\nlong body that is not counted")
	writeFile(t, filepath.Join(claude, "skills", "synced", "x", "deep", skillFile), "---\nname: deep\n---\n")
	writeFile(t, filepath.Join(claude, "skills", "notaskill", "README.md"), "x")
	writeFile(t, filepath.Join(claude, "skills", ".hidden", skillFile), "x")
	got, err := LooseSkills(claude)
	if err != nil || len(got) != 1 {
		t.Fatalf("only the one loose skill: %+v %v", got, err)
	}
	g := got[0]
	if g.ID != "glitchtip" || g.Kind != CapSkill || g.Source != SourceThisMachine || g.Description != "Read GlitchTip issues" || g.Cost != textTokens("glitchtip Read GlitchTip issues") {
		t.Fatalf("skill entry = %+v", g)
	}
	if none, err := LooseSkills(t.TempDir()); err != nil || none != nil {
		t.Fatalf("no skills folder is none: %v %v", none, err)
	}

	plugin := t.TempDir()
	writeFile(t, filepath.Join(plugin, "skills", "check", skillFile), "---\nname: check\ndescription: gates\n---\n")
	writeFile(t, filepath.Join(plugin, "commands", "ship.md"), "---\ndescription: ship it\n---\n")
	writeFile(t, filepath.Join(plugin, ".mcp.json"), `{"mcpServers":{"a":{},"b":{}}}`)
	listed := textTokens("check gates") + textTokens(" ship it")
	if got := pluginCost(plugin, -1); got != listed+2*unknownServerTools*toolTokens {
		t.Fatalf("unknown tools: two servers of %d tools: %d", unknownServerTools, got)
	}
	if got := pluginCost(plugin, 3); got != listed+3*toolTokens {
		t.Fatalf("three counted tools: %d", got)
	}
}

func TestProbeCandidates(t *testing.T) {
	synced := t.TempDir()
	writeFile(t, filepath.Join(synced, fileManifest), `{"name":"marketing"}`)
	p := ProbeResult{
		Plugins: []ProbePlugin{
			{Name: "qa", Path: "/p/qa", Source: "qa@personal", Version: "0.8.0"},
			{Name: "marketing", Path: synced, Source: "marketing@synced", Version: "1.2.0"},
			{Name: "cc-plugin-x", Path: "builtin", Source: "cc-plugin-x@builtin"},
		},
		MCPServers: []ProbeServer{
			{Name: "claude.ai Gmail", Source: "claudeai", Status: "connected"},
			{Name: "claude.ai Lusha", Source: "claudeai", Status: "needs-auth"},
			{Name: "plugin:marketing:slack", Source: "plugin", Status: "needs-auth"},
			{Name: "claude-peers", Source: "user", Status: "connected"},
		},
		Tools: []string{"mcp__claude_ai_Gmail__search", "mcp__claude_ai_Gmail__send", "mcp__plugin_marketing_slack__post"},
	}
	got, skipped := probeCandidates(p)
	byID := map[string]Capability{}
	for _, c := range got {
		byID[c.ID] = c
	}
	if len(got) != 3 {
		t.Fatalf("one synced plugin and two connectors: %+v", got)
	}
	if m := byID["claude.ai-marketing"]; m.Source != SourceClaudeAI || m.Path != synced || m.Version != "1.2.0" || m.Cost != toolTokens {
		t.Fatalf("synced plugin = %+v", m)
	}
	writeFile(t, filepath.Join(synced, ".mcp.json"), `{"mcpServers":{"gmail":{}}}`)
	p.Tools = nil
	if got, _ := probeCandidates(p); got[0].Cost != unknownServerTools*toolTokens {
		t.Fatalf("a plugin whose servers showed no tools counts them unknown: %+v", got[0])
	}
	if g := byID["claude.ai-Gmail"]; g.Kind != CapConnector || g.Connector != "claude.ai Gmail" || g.Cost != 2*toolTokens {
		t.Fatalf("gmail = %+v", g)
	}
	if l := byID["claude.ai-Lusha"]; l.Cost != unknownServerTools*toolTokens {
		t.Fatalf("a connector not connected has unknown tools: %+v", l)
	}
	if len(skipped) != 2 || !strings.HasPrefix(skipped[0], "cc-plugin-x") || !strings.HasPrefix(skipped[1], "claude-peers") {
		t.Fatalf("skipped = %v", skipped)
	}
	if out, err := parseProbe([]byte("warning: x\n{\"plugins\":[{\"name\":\"qa\"}]}\n")); err != nil || out.Plugins[0].Name != "qa" {
		t.Fatalf("parseProbe reads the last line: %+v %v", out, err)
	}
	if _, err := parseProbe([]byte("")); err == nil {
		t.Fatal("no output is an error")
	}
}

func TestImportCapabilities_WithProbe(t *testing.T) {
	s := testStore(t)
	claude := t.TempDir()
	writeFile(t, filepath.Join(claude, "skills", "improve", skillFile), "---\nname: improve\ndescription: survey\n---\n")
	probe := &ProbeResult{MCPServers: []ProbeServer{{Name: "claude.ai Gmail", Source: "claudeai", Status: "connected"}}, Tools: []string{}}
	res, err := s.ImportCapabilities(claude, probe)
	if err != nil || !slices.Equal(res.Added, []string{"claude.ai-Gmail", "improve"}) {
		t.Fatalf("import = %+v %v", res, err)
	}
	if again, _ := s.ImportCapabilities(claude, probe); len(again.Added) != 0 || len(again.Skipped) != 0 {
		t.Fatalf("a second import finds everything there: %+v", again)
	}
	if res, _ := s.ImportCapabilities(claude, nil); len(res.Added) != 0 {
		t.Fatalf("without a probe the loose skills still import: %+v", res)
	}
}

func TestPropose_SourcePluginFolder(t *testing.T) {
	catalog := []Capability{
		{ID: "qa", Kind: CapPlugin, Path: "/ckout/qa", Source: "personal", Commit: "abc"},
		{ID: "kb", Kind: CapPlugin, Path: "/ckout/kb", Source: "personal", Commit: "abc"},
		{ID: "improve", Kind: CapSkill, Path: "/s/improve"},
	}
	var h home
	h.settings.EnabledPlugins = map[string]bool{"qa@personal": true, "kb@personal": false, "improve@x": true}
	got := propose([]home{h}, catalog)
	if !slices.Equal(got.Capabilities, []string{"qa"}) {
		t.Fatalf("a home enabling qa@personal wants the source's qa, not a disabled kb or a skill: %v", got.Capabilities)
	}
}
