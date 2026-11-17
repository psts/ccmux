package agent

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// gitT runs git in dir for a test, failing it on error.
func gitT(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v %s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// marketRepo is a local marketplace: a work tree to change and commit in,
// and the bare repo a source clones from. It lists qa (manifest 1.0.0, one
// skill), kb (no version anywhere, so its folder's commit) and an external
// plugin from a url.
type marketRepo struct{ work, bare string }

func newMarketRepo(t *testing.T) marketRepo {
	t.Helper()
	root := t.TempDir()
	m := marketRepo{work: filepath.Join(root, "work"), bare: filepath.Join(root, "market.git")}
	writeFile(t, filepath.Join(m.work, ".claude-plugin", "marketplace.json"), `{"name":"personal","plugins":[
		{"name":"qa","source":"./plugins/qa","description":"gates"},
		{"name":"kb","source":"./plugins/kb"},
		{"name":"far","source":{"source":"url","url":"https://example.com/far.git"}},
		{"name":"escape","source":"../outside"}]}`)
	writeFile(t, filepath.Join(m.work, "plugins", "qa", fileManifest), `{"name":"qa","version":"1.0.0"}`)
	writeFile(t, filepath.Join(m.work, "plugins", "qa", "skills", "check", skillFile), "---\nname: check\ndescription: run the gates\n---\nbody")
	writeFile(t, filepath.Join(m.work, "plugins", "kb", fileManifest), `{"name":"kb"}`)
	gitT(t, root, "init", "--quiet", "-b", "main", m.work)
	m.commit(t, "first")
	gitT(t, root, "clone", "--quiet", "--bare", m.work, m.bare)
	gitT(t, m.work, "remote", "add", "origin", m.bare)
	return m
}

func (m marketRepo) commit(t *testing.T, msg string) {
	t.Helper()
	gitT(t, m.work, "add", "-A")
	gitT(t, m.work, "commit", "--quiet", "-m", msg)
}

func (m marketRepo) push(t *testing.T) {
	t.Helper()
	gitT(t, m.work, "push", "--quiet", "origin", "main")
}

func entryOf(t *testing.T, s *Store, id string) Capability {
	t.Helper()
	list, err := s.Capabilities()
	if err != nil {
		t.Fatal(err)
	}
	i := slices.IndexFunc(list, func(c Capability) bool { return c.ID == id })
	if i < 0 {
		t.Fatalf("no entry %s in %+v", id, list)
	}
	return list[i]
}

func TestSources_SeedsAndChecks(t *testing.T) {
	s := testStore(t)
	list, err := s.Sources()
	if err != nil || len(list) != 3 || list[0].ID != "claude-plugins-official" || list[1].ID != "personal" {
		t.Fatalf("no file yet is the three seeds: %+v %v", list, err)
	}
	for _, bad := range []CapSource{
		{ID: "x", URL: "http://example.com/x"},
		{ID: "x", URL: "https://user:token@github.com/x/y"},
		{ID: "x", URL: "relative/path"},
		{ID: "bad id", URL: "https://github.com/x/y"},
		{ID: pinsFolder, URL: "https://github.com/x/y"},
	} {
		if _, err := s.AddSource(bad); err == nil {
			t.Errorf("%+v should be refused", bad)
		}
	}
	if _, err := s.AddSource(CapSource{ID: "personal", URL: "https://github.com/x/y"}); !errors.Is(err, ErrSourceExists) {
		t.Fatalf("a taken id is ErrSourceExists, got %v", err)
	}
	for _, src := range seedSources {
		if err := s.RemoveSource(src.ID); err != nil {
			t.Fatal(err)
		}
	}
	if list, _ := s.Sources(); len(list) != 0 {
		t.Fatalf("removing every seed keeps them gone: %+v", list)
	}
}

func TestSources_UpdateSyncsPlugins(t *testing.T) {
	s, m := testStore(t), newMarketRepo(t)
	ctx := context.Background()
	if _, err := s.AddSource(CapSource{ID: "local", URL: m.bare}); err != nil {
		t.Fatal(err)
	}
	res, err := s.UpdateSource(ctx, "local")
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(res.Added, []string{"kb", "qa"}) && !slices.Equal(res.Added, []string{"qa", "kb"}) {
		t.Fatalf("added = %v", res.Added)
	}
	if !slices.Equal(res.External, []string{"far", "escape"}) {
		t.Fatalf("a url source and a folder outside the checkout are external: %v", res.External)
	}
	qa := entryOf(t, s, "qa")
	if qa.Version != "1.0.0" || qa.Source != "local" || qa.Path != filepath.Join(s.checkoutDir("local"), "plugins", "qa") || qa.Cost == 0 || qa.Description != "gates" {
		t.Fatalf("qa entry = %+v", qa)
	}
	if kb := entryOf(t, s, "kb"); len(kb.Version) < 7 {
		t.Fatalf("a plugin with no version gets its folder's commit: %+v", kb)
	}
	kbVersion := entryOf(t, s, "kb").Version

	writeFile(t, filepath.Join(m.work, "plugins", "qa", fileManifest), `{"name":"qa","version":"1.1.0"}`)
	m.commit(t, "qa 1.1.0")
	m.push(t)
	res, err = s.UpdateSource(ctx, "local")
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Changed) != 1 || res.Changed[0] != (VersionChange{ID: "qa", From: "1.0.0", To: "1.1.0"}) || len(res.Added) != 0 {
		t.Fatalf("only qa moved: %+v", res)
	}
	if entryOf(t, s, "kb").Version != kbVersion {
		t.Fatal("kb's folder did not change, so its version must not either")
	}
	src, _ := s.source("local")
	if src.Marketplace != "personal" || src.Plugins["qa"] != "1.1.0" || res.From == src.Commit {
		t.Fatalf("source record = %+v (from %s)", src, res.From)
	}

	// The source rewrites its history: the next update follows it.
	writeFile(t, filepath.Join(m.work, "plugins", "qa", fileManifest), `{"name":"qa","version":"1.2.0"}`)
	gitT(t, m.work, "add", "-A")
	gitT(t, m.work, "commit", "--quiet", "--amend", "-m", "qa 1.2.0 instead")
	gitT(t, m.work, "push", "--quiet", "--force", "origin", "main")
	res, err = s.UpdateSource(ctx, "local")
	if err != nil || len(res.Changed) != 1 || res.Changed[0].To != "1.2.0" || entryOf(t, s, "kb").Version != kbVersion {
		t.Fatalf("a force-pushed source still updates, kb untouched: %+v %v", res, err)
	}
}

func TestImport_AfterSourceSync(t *testing.T) {
	for _, id := range []string{"local", "other"} {
		t.Run(id, func(t *testing.T) { importAfterSync(t, id) })
	}
}

// importAfterSync syncs a source named id whose marketplace calls itself
// "personal", then imports qa@personal and kb@other. "other" as the
// source's id must not make kb@other look like the source's kb.
func importAfterSync(t *testing.T, id string) {
	s, m := testStore(t), newMarketRepo(t)
	s.AddSource(CapSource{ID: id, URL: m.bare})
	if _, err := s.UpdateSource(context.Background(), id); err != nil {
		t.Fatal(err)
	}
	claude := t.TempDir()
	installs := filepath.Join(claude, "plugins", "cache")
	writeFile(t, InstalledPluginsPath(claude), `{"plugins":{
		"qa@personal":[{"installPath":"`+filepath.Join(installs, "qa")+`","lastUpdated":"1"}],
		"kb@other":[{"installPath":"`+filepath.Join(installs, "kb")+`","lastUpdated":"1"}]}}`)
	res, err := s.ImportCapabilities(claude, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(res.Skipped, []string{"kb"}) || len(res.Added) != 0 {
		t.Fatalf("qa@personal is the source's qa; kb@other is another plugin on a taken id: %+v", res)
	}
	if qa := entryOf(t, s, "qa"); !fromCheckout(qa) {
		t.Fatalf("the import never replaces the source's entry: %+v", qa)
	}
}

func TestSources_SyncedCopyAndSourceBothList(t *testing.T) {
	s, m := testStore(t), newMarketRepo(t)
	synced := filepath.Join(t.TempDir(), "qa")
	writeFile(t, filepath.Join(synced, fileManifest), `{"name":"qa"}`)
	probe := &ProbeResult{Plugins: []ProbePlugin{{Name: "qa", Path: synced, Source: "qa@synced", Version: "0.1"}}, Tools: []string{}}
	if _, err := s.ImportCapabilities(t.TempDir(), probe); err != nil {
		t.Fatal(err)
	}
	s.AddSource(CapSource{ID: "local", URL: m.bare})
	res, err := s.UpdateSource(context.Background(), "local")
	if err != nil || !slices.Contains(res.Added, "qa") || len(res.Skipped) != 0 {
		t.Fatalf("the source's qa is added beside claude.ai's: %+v %v", res, err)
	}
	if c := entryOf(t, s, "claude.ai-qa"); c.Path != synced || c.Source != SourceClaudeAI {
		t.Fatalf("claude.ai's copy keeps its own row: %+v", c)
	}
}

func TestSources_CloneLeavesNoHalfCheckout(t *testing.T) {
	s, m := testStore(t), newMarketRepo(t)
	ctx := context.Background()
	s.AddSource(CapSource{ID: "local", URL: m.bare})
	s.AddSource(CapSource{ID: "broken", URL: filepath.Join(t.TempDir(), "none.git")})
	if _, err := s.UpdateSource(ctx, "broken"); err == nil {
		t.Fatal("a url that does not clone fails")
	}
	if _, err := os.Stat(s.checkoutDir("broken")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("a failed clone leaves no folder behind")
	}
	// What a clone cut off before the rename used to leave: a folder with
	// no repo in it. The next update clones over it.
	writeFile(t, filepath.Join(s.checkoutDir("local"), "junk"), "x")
	if res, err := s.UpdateSource(ctx, "local"); err != nil || len(res.Added) != 2 {
		t.Fatalf("an update clones over a leftover folder: %+v %v", res, err)
	}
	left, _ := filepath.Glob(filepath.Join(s.checkoutsDir(), ".*.clone-*"))
	if len(left) != 0 {
		t.Fatalf("no clone folders left: %v", left)
	}
}

func TestSources_PinHoldsTheFolder(t *testing.T) {
	s, m := testStore(t), newMarketRepo(t)
	ctx := context.Background()
	s.AddSource(CapSource{ID: "local", URL: m.bare})
	if _, err := s.UpdateSource(ctx, "local"); err != nil {
		t.Fatal(err)
	}
	pinned, err := s.PinCapability(ctx, "qa", true)
	if err != nil || !pinned.Pinned || !strings.HasPrefix(pinned.Path, s.pinsDir("local")) {
		t.Fatalf("pin = %+v %v", pinned, err)
	}
	writeFile(t, filepath.Join(m.work, "plugins", "qa", fileManifest), `{"name":"qa","version":"2.0.0"}`)
	m.commit(t, "qa 2")
	m.push(t)
	res, err := s.UpdateSource(ctx, "local")
	if err != nil || len(res.Changed) != 0 || len(res.Behind) != 1 || res.Behind[0].To != "2.0.0" {
		t.Fatalf("a pinned entry is behind, not changed: %+v %v", res, err)
	}
	qa := entryOf(t, s, "qa")
	if qa.Version != "1.0.0" || qa.Path != pinned.Path || manifestVersion(qa.Path) != "1.0.0" {
		t.Fatalf("the pin's folder stays at 1.0.0: %+v (folder says %s)", qa, manifestVersion(qa.Path))
	}
	if manifestVersion(filepath.Join(s.checkoutDir("local"), "plugins", "qa")) != "2.0.0" {
		t.Fatal("the checkout itself moved on")
	}
	free, err := s.PinCapability(ctx, "qa", false)
	if err != nil || free.Pinned || free.Version != "2.0.0" || free.Path != filepath.Join(s.checkoutDir("local"), "plugins", "qa") {
		t.Fatalf("unpin goes back to the checkout at the source's version: %+v %v", free, err)
	}
	s.SaveCapability(Capability{ID: "door", Kind: CapMCP, Server: &mcpEntry{URL: "http://d/mcp"}})
	if _, err := s.PinCapability(ctx, "door", true); !errors.Is(err, ErrNotPinnable) {
		t.Fatalf("an entry from no source cannot pin: %v", err)
	}
}

func TestSources_RemoveAndAdopt(t *testing.T) {
	s, m := testStore(t), newMarketRepo(t)
	ctx := context.Background()
	// An import made qa from the installed plugin before the source came.
	if _, err := s.SaveCapability(Capability{ID: "qa", Kind: CapPlugin, Plugin: "qa@personal"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SaveCapability(Capability{ID: "kb", Kind: CapPlugin, Plugin: "kb@personal"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Save(Definition{Name: "radar", Description: "d", Harness: "claude", Capabilities: []string{"kb"}}); err != nil {
		t.Fatal(err)
	}
	s.AddSource(CapSource{ID: "local", URL: m.bare})
	res, err := s.UpdateSource(ctx, "local")
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(res.Added, "qa") || !fromCheckout(entryOf(t, s, "qa")) {
		t.Fatalf("an unused import of the same plugin is re-pointed at the checkout: %+v", res)
	}
	if kb := entryOf(t, s, "kb"); kb.Plugin != "kb@personal" || len(res.Skipped) != 1 || !strings.Contains(res.Skipped[0], "agent radar") {
		t.Fatalf("an import in use stays as it is: %+v %+v", kb, res.Skipped)
	}
	s.Save(Definition{Name: "radar", Description: "d", Harness: "claude", Capabilities: []string{"qa"}})
	if err := s.RemoveSource("local"); !errors.Is(err, ErrCapabilityInUse) {
		t.Fatalf("a source whose entry is used stays: %v", err)
	}
	s.Save(Definition{Name: "radar", Description: "d", Harness: "claude"})
	if err := s.RemoveSource("local"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(s.checkoutDir("local")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("the checkout goes with its source")
	}
	list, _ := s.Capabilities()
	if len(list) != 1 || list[0].ID != "kb" {
		t.Fatalf("only the source's own entries go: %+v", list)
	}
}
