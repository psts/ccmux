package agent

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func testStore(t *testing.T) *Store {
	s := NewStore(filepath.Join(t.TempDir(), "agents"))
	s.now = func() time.Time { return time.Date(2026, 9, 5, 12, 0, 0, 0, time.UTC) }
	return s
}

func TestRejectRules(t *testing.T) {
	good := Definition{Name: "x-poster", Description: "Posts X threads about releases."}
	if msg := Reject(good); msg != "" {
		t.Fatalf("good refused: %s", msg)
	}
	cases := map[string]Definition{
		"bad name":   {Name: "X Poster", Description: "d"},
		"short name": {Name: "ab", Description: "d"},
		"no desc":    {Name: "x-poster"},
		"long desc":  {Name: "x-poster", Description: strings.Repeat("x", 161)},
		"perm value": {Name: "x-poster", Description: "d", Permissions: Permissions{Bash: "maybe"}},
		"memory":     {Name: "x-poster", Description: "d", Memory: "global"},
		"start":      {Name: "x-poster", Description: "d", Start: "resume"},
		"negative":   {Name: "x-poster", Description: "d", IdleExitMinutes: -1},
	}
	for name, d := range cases {
		if Reject(d) == "" {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestSaveCreatesLayoutWithDefaultsAndVersion(t *testing.T) {
	s := testStore(t)
	got, err := s.Save(Definition{Name: "x-poster", Icon: "𝕏", Description: "Posts X threads.", Instructions: "# Role\n\nPost things.\n"})
	if err != nil {
		t.Fatal(err)
	}
	if got.Version != "1.0.0" || got.Harness != DefaultHarness || got.Permissions.Bash != "ask" || got.IdleExitMinutes != 0 || got.Start != "fresh" {
		t.Fatalf("defaults not applied: %+v", got)
	}
	dir := s.Dir("x-poster")
	for _, f := range []string{"agent.json", "AGENTS.md", "CLAUDE.md", "CHANGELOG.md", "mcp.json", ".claude-plugin/plugin.json"} {
		if _, err := os.Stat(filepath.Join(dir, f)); err != nil {
			t.Errorf("missing %s", f)
		}
	}
	for _, d := range []string{"skills", "knowledge"} {
		if fi, err := os.Stat(filepath.Join(dir, d)); err != nil || !fi.IsDir() {
			t.Errorf("missing dir %s", d)
		}
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "CLAUDE.md")); string(b) != "@AGENTS.md\n" {
		t.Errorf("CLAUDE.md should import AGENTS.md, got %q", b)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "agent.json")); strings.Contains(string(b), "instructions") || strings.Contains(string(b), "\"version\"") {
		t.Error("agent.json must not carry instructions or version; they live in their own files")
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "CHANGELOG.md")); !strings.Contains(string(b), "1.0.0 2026-09-05: created") {
		t.Errorf("changelog: %q", b)
	}
}

func TestSaveBumpsOnlyWhenGeneratedContentChanges(t *testing.T) {
	s := testStore(t)
	d := Definition{Name: "kb-writer", Description: "Writes KB articles.", Instructions: "v1\n"}
	if _, err := s.Save(d); err != nil {
		t.Fatal(err)
	}
	// Same definition, empty instructions: nothing changes, no bump, AGENTS.md kept.
	again, err := s.Save(Definition{Name: "kb-writer", Description: "Writes KB articles."})
	if err != nil {
		t.Fatal(err)
	}
	if again.Version != "1.0.0" || again.Instructions != "v1\n" {
		t.Fatalf("no-op save changed things: %+v", again)
	}
	// New instructions: bump.
	d.Instructions = "v2\n"
	bumped, err := s.Save(d)
	if err != nil {
		t.Fatal(err)
	}
	if bumped.Version != "1.0.1" || bumped.Instructions != "v2\n" {
		t.Fatalf("expected 1.0.1 with v2, got %+v", bumped)
	}
	// Field change alone: bump.
	d.KeepAlive = true
	if b, _ := s.Save(d); b.Version != "1.0.2" {
		t.Fatalf("field change should bump, got %s", b.Version)
	}
}

func TestSaveNeverTouchesHumanFiles(t *testing.T) {
	s := testStore(t)
	if _, err := s.Save(Definition{Name: "x-poster", Description: "d", Instructions: "r\n"}); err != nil {
		t.Fatal(err)
	}
	dir := s.Dir("x-poster")
	os.WriteFile(filepath.Join(dir, "mcp.json"), []byte(`{"x":{"command":"x-mcp"}}`), 0o644)
	os.MkdirAll(filepath.Join(dir, "skills", "post"), 0o755)
	os.WriteFile(filepath.Join(dir, "skills", "post", "SKILL.md"), []byte("---\nname: post\n---\n"), 0o644)
	os.WriteFile(filepath.Join(dir, "knowledge", "style.md"), []byte("tone"), 0o644)
	if _, err := s.Save(Definition{Name: "x-poster", Description: "d2", Instructions: "r2\n"}); err != nil {
		t.Fatal(err)
	}
	for f, want := range map[string]string{"mcp.json": `{"x":{"command":"x-mcp"}}`, "skills/post/SKILL.md": "---\nname: post\n---\n", "knowledge/style.md": "tone"} {
		if b, _ := os.ReadFile(filepath.Join(dir, f)); string(b) != want {
			t.Errorf("%s was rewritten: %q", f, b)
		}
	}
}

func TestListGetDelete(t *testing.T) {
	s := testStore(t)
	if l, err := s.List(); err != nil || len(l) != 0 {
		t.Fatalf("empty root: %v %v", l, err)
	}
	s.Save(Definition{Name: "a-one", Description: "d"})
	s.Save(Definition{Name: "b-two", Description: "d"})
	os.MkdirAll(filepath.Join(s.Root, ".hidden"), 0o755)
	l, err := s.List()
	if err != nil || len(l) != 2 || l[0].Name != "a-one" {
		t.Fatalf("list: %+v %v", l, err)
	}
	if _, err := s.Get("nope"); err != ErrNotFound {
		t.Fatalf("missing should be ErrNotFound, got %v", err)
	}
	if err := s.Delete("a-one"); err != nil {
		t.Fatal(err)
	}
	if err := s.Delete("a-one"); err != ErrNotFound {
		t.Fatalf("second delete should be ErrNotFound, got %v", err)
	}
	if err := s.Delete("../etc"); err == nil {
		t.Fatal("path-ish name must be refused")
	}
}

func TestListFailsLoudOnCorruptFolder(t *testing.T) {
	s := testStore(t)
	s.Save(Definition{Name: "good", Description: "d"})
	os.MkdirAll(s.Dir("broken"), 0o755)
	os.WriteFile(filepath.Join(s.Dir("broken"), "agent.json"), []byte("{not json"), 0o644)
	if _, err := s.List(); err == nil || !strings.Contains(err.Error(), "broken") {
		t.Fatalf("corrupt folder must fail the list and name itself, got %v", err)
	}
}

func TestBootstrapOnce(t *testing.T) {
	repo := t.TempDir()
	d := Definition{Name: "x-poster", Description: "Posts X threads."}
	dir, created, err := Bootstrap(repo, d)
	if err != nil || !created || dir != filepath.Join(repo, ".ccmux", "agents", "x-poster") {
		t.Fatalf("%s %v %v", dir, created, err)
	}
	for _, f := range []string{"AGENTS.md", "CLAUDE.md", "memory/MEMORY.md", "log.md", ".claude/skills"} {
		if _, err := os.Stat(filepath.Join(dir, f)); err != nil {
			t.Errorf("missing %s", f)
		}
	}
	os.WriteFile(filepath.Join(dir, "AGENTS.md"), []byte("edited by human\n"), 0o644)
	_, created, err = Bootstrap(repo, d)
	if err != nil || created {
		t.Fatalf("second bootstrap must be a no-op: %v %v", created, err)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "AGENTS.md")); string(b) != "edited by human\n" {
		t.Fatal("bootstrap overwrote a human-owned file")
	}
}

func TestNamesThatAreNotFolderSegmentsNeverReachTheFilesystem(t *testing.T) {
	s := testStore(t)
	s.Save(Definition{Name: "real", Description: "d"})
	for _, bad := range []string{"../real", "../../etc", "a/b", ".hidden", "UPPER"} {
		if _, err := s.Get(bad); err != ErrNotFound {
			t.Errorf("Get(%q) = %v, want ErrNotFound", bad, err)
		}
		if _, _, err := Bootstrap(t.TempDir(), Definition{Name: bad, Description: "d"}); err == nil {
			t.Errorf("Bootstrap(%q) accepted", bad)
		}
		if err := s.WriteInstanceConfig(Definition{Name: bad}, t.TempDir()); err == nil {
			t.Errorf("WriteInstanceConfig(%q) accepted", bad)
		}
	}
}

func TestIdleZeroSurvivesAndDefaultsApplyOnlyToNewAgents(t *testing.T) {
	if Defaults().IdleExitMinutes != DefaultIdleExitMinutes {
		t.Fatal("a new agent starts with the idle default")
	}
	s := testStore(t)
	got, err := s.Save(Definition{Name: "never-sleeps", Description: "d", IdleExitMinutes: 0})
	if err != nil || got.IdleExitMinutes != 0 {
		t.Fatalf("0 must mean never, got %d (%v)", got.IdleExitMinutes, err)
	}
	os.WriteFile(filepath.Join(s.Dir("never-sleeps"), ".claude-plugin", "plugin.json"), []byte("{bad"), 0o644)
	if _, err := s.Get("never-sleeps"); err == nil {
		t.Fatal("a corrupt manifest must be an error, not version \"\"")
	}
}
