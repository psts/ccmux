package agent

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestParseSkillURL(t *testing.T) {
	for raw, want := range map[string]GitSource{
		"https://github.com/vercel-labs/skills/tree/main/skills/find-skills": {Repo: "https://github.com/vercel-labs/skills.git", Ref: "main", Path: "skills/find-skills"},
		"https://github.com/o/r/blob/dev/a/b/SKILL.md":                       {Repo: "https://github.com/o/r.git", Ref: "dev", Path: "a/b"},
		"https://github.com/o/r":                                             {Repo: "https://github.com/o/r.git"},
		"https://gitlab.com/o/r.git#v2:skills/x":                             {Repo: "https://gitlab.com/o/r.git", Ref: "v2", Path: "skills/x"},
		"https://gitlab.com/o/r.git#skills/x":                                {Repo: "https://gitlab.com/o/r.git", Path: "skills/x"},
		"git@github.com:o/r.git":                                             {Repo: "git@github.com:o/r.git"},
	} {
		got, err := ParseSkillURL(raw)
		if err != nil || got != want {
			t.Errorf("%s → %+v, %v (want %+v)", raw, got, err, want)
		}
	}
	if _, err := ParseSkillURL("https://github.com/only-owner"); err == nil {
		t.Error("an owner without a repo is not a source")
	}
}

// seedSkillsRepo makes a local git repo holding skills/<name>/SKILL.md plus
// a helper file, and returns its path.
func seedSkillsRepo(t *testing.T, name, description string) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	repo := t.TempDir()
	dir := filepath.Join(repo, "skills", name)
	os.MkdirAll(filepath.Join(dir, "refs"), 0o755)
	os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte("---\nname: "+name+"\ndescription: "+description+"\n---\n# "+name+"\n"), 0o644)
	os.WriteFile(filepath.Join(dir, "refs", "notes.md"), []byte("more"), 0o644)
	os.WriteFile(filepath.Join(repo, "README.md"), []byte("not a skill"), 0o644)
	for _, args := range [][]string{{"init", "-q", "-b", "main"}, {"add", "."}, {"-c", "user.name=t", "-c", "user.email=t@t", "commit", "-q", "-m", "seed"}} {
		cmd := exec.Command("git", args...)
		cmd.Dir = repo
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %s", args, out)
		}
	}
	return repo
}

func TestSkillsFromGitAndFiles(t *testing.T) {
	s := NewStore(filepath.Join(t.TempDir(), "agents"))
	if _, err := s.Save(Definition{Name: "x-poster", Description: "d"}); err != nil {
		t.Fatal(err)
	}
	repo := seedSkillsRepo(t, "post-thread", "Posts a thread.")
	source := repo + "#main:skills/post-thread"
	src, err := ParseSkillURL(source)
	if err != nil {
		t.Fatal(err)
	}
	sk, err := s.InstallSkillFromGit(context.Background(), "x-poster", src, source)
	if err != nil {
		t.Fatal(err)
	}
	if sk.Name != "post-thread" || sk.Description != "Posts a thread." || sk.Source != source || sk.Files != 2 {
		t.Fatalf("installed: %+v", sk)
	}
	if _, err := os.Stat(filepath.Join(s.Dir("x-poster"), "skills", "post-thread", "refs", "notes.md")); err != nil {
		t.Error("the whole skill folder comes along, not just SKILL.md")
	}
	if _, err := os.Stat(filepath.Join(s.Dir("x-poster"), "skills", "README.md")); err == nil {
		t.Error("only the named folder is installed")
	}
	d, _ := s.Get("x-poster")
	if d.Version != "1.0.1" {
		t.Errorf("a skill change bumps the base: %s", d.Version)
	}
	// Files dropped on the lens: the frontmatter name wins over the given one.
	sk2, err := s.PutSkillFiles("x-poster", "given", map[string]string{"SKILL.md": "---\nname: review-draft\ndescription: Reviews.\n---\n", "../escape.md": "x"})
	if err != nil || sk2.Name != "review-draft" || sk2.Description != "Reviews." || sk2.Source != "" {
		t.Fatalf("from files: %+v %v", sk2, err)
	}
	if _, err := os.Stat(filepath.Join(s.Dir("x-poster"), "skills", "escape.md")); err == nil {
		t.Error("a relative path must not escape the skill folder")
	}
	if _, err := s.PutSkillFiles("x-poster", "nope", map[string]string{"notes.md": "x"}); err == nil {
		t.Error("no SKILL.md, no skill")
	}
	list, err := s.Skills("x-poster")
	if err != nil || len(list) != 2 || list[0].Name != "post-thread" || list[1].Name != "review-draft" {
		t.Fatalf("list: %+v %v", list, err)
	}
	text, err := s.SkillText("x-poster", "post-thread")
	if err != nil || !strings.Contains(text, "# post-thread") {
		t.Fatalf("text: %q %v", text, err)
	}
	// Update re-fetches from the recorded source; a hand-written one cannot.
	if _, err := s.UpdateSkill(context.Background(), "x-poster", "post-thread"); err != nil {
		t.Fatalf("update: %v", err)
	}
	if _, err := s.UpdateSkill(context.Background(), "x-poster", "review-draft"); err == nil {
		t.Error("a hand-written skill has no source to update from")
	}
	if err := s.DeleteSkill("x-poster", "post-thread"); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteSkill("x-poster", "post-thread"); err != ErrNotFound {
		t.Errorf("second delete = %v", err)
	}
	d, _ = s.Get("x-poster")
	if d.Version != "1.0.4" {
		t.Errorf("every skill change bumps: %s", d.Version)
	}
}

func TestMCPSnippets(t *testing.T) {
	s := NewStore(filepath.Join(t.TempDir(), "agents"))
	s.Save(Definition{Name: "x-poster", Description: "d"})
	names, err := s.AddMCP("x-poster", []byte(`{"mcpServers":{"github":{"command":"npx","args":["-y","@modelcontextprotocol/server-github"],"env":{"GITHUB_TOKEN":"$GITHUB_TOKEN"}}}}`))
	if err != nil || len(names) != 1 || names[0] != "github" {
		t.Fatalf("wrapped: %v %v", names, err)
	}
	if _, err := s.AddMCP("x-poster", []byte(`{"fs":{"command":"mcp-fs"}}`)); err != nil {
		t.Fatalf("bare: %v", err)
	}
	for _, bad := range []string{`{}`, `{"x":{"args":["a"]}}`, `not json`} {
		if _, err := s.AddMCP("x-poster", []byte(bad)); err == nil {
			t.Errorf("accepted %q", bad)
		}
	}
	list, err := s.MCPServers("x-poster")
	if err != nil || len(list) != 2 || list[0].Name != "fs" || list[1].Name != "github" || list[1].Env["GITHUB_TOKEN"] != "$GITHUB_TOKEN" {
		t.Fatalf("list: %+v %v", list, err)
	}
	if err := s.RemoveMCP("x-poster", "fs"); err != nil {
		t.Fatal(err)
	}
	if err := s.RemoveMCP("x-poster", "fs"); err != ErrNotFound {
		t.Errorf("second remove = %v", err)
	}
	// The instance config carries the servers, the skills folder and the
	// base's plugins.
	d, _ := s.Get("x-poster")
	d.Plugins = []string{"opencode-foo"}
	mcp, _ := ReadMCP(s.Dir("x-poster"))
	body, _ := OpencodeInstanceConfig(d, s.Dir("x-poster"), mcp, []string{"/p/ccmux.ts"}, "")
	for _, want := range []string{`"github"`, `"opencode-foo"`, `"/p/ccmux.ts"`, `"skills"`, filepath.Join(s.Dir("x-poster"), "skills")} {
		if !strings.Contains(string(body), want) {
			t.Errorf("config missing %s:\n%s", want, body)
		}
	}
}
