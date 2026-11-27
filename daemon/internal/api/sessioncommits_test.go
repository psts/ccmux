package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"ccmux.dev/ccmuxd/internal/store"
)

func TestPickRangesByBranchModel(t *testing.T) {
	om, lm := refOrigin("main"), refHead("main")
	all := map[string]bool{refDev: true, refOriginDev: true, lm: true, om: true}
	cases := []struct {
		name  string
		has   map[string]bool
		head  string
		trunk string
		tag   string
		want  []commitRange
	}{
		{"dev and origin/dev", all, "dev", "main", "", []commitRange{
			{"unpushed", "Not pushed", refOriginDev, refDev},
			{"pushed", "Pushed, not shipped", om, refOriginDev},
		}},
		{"a task's branch in a dev repo", all, "task/9-x", "main", "", []commitRange{
			{"branch", "On this branch (task/9-x)", refDev, "HEAD"},
			{"unpushed", "Not pushed", refOriginDev, refDev},
			{"pushed", "Pushed, not shipped", om, refOriginDev},
		}},
		{"on main in a dev repo: no branch group", all, "main", "main", "", []commitRange{
			{"unpushed", "Not pushed", refOriginDev, refDev},
			{"pushed", "Pushed, not shipped", om, refOriginDev},
		}},
		{"dev never pushed: all of it since main", map[string]bool{refDev: true, lm: true}, "dev", "main", "", []commitRange{
			{"unpushed", "Not pushed", lm, refDev},
		}},
		{"only origin/dev, no main at all", map[string]bool{refOriginDev: true}, "", "", "", nil},
		{"main only, with a tag", map[string]bool{lm: true, om: true}, "main", "main", "refs/tags/v1.2", []commitRange{
			{"unpushed", "Not pushed", om, lm},
			{"release", "Since the last release", "refs/tags/v1.2", om},
		}},
		{"main only, no tag", map[string]bool{lm: true, om: true}, "main", "main", "", []commitRange{
			{"unpushed", "Not pushed", om, lm},
		}},
		{"main only, never pushed: no group is the whole history", map[string]bool{lm: true}, "main", "main", "", nil},
		{"main only, a worktree's branch", map[string]bool{lm: true, om: true}, "task/3", "main", "", []commitRange{
			{"branch", "On this branch (task/3)", lm, "HEAD"},
			{"unpushed", "Not pushed", om, lm},
		}},
		{"detached HEAD has no branch group", map[string]bool{lm: true, om: true}, "", "main", "", []commitRange{
			{"unpushed", "Not pushed", om, lm},
		}},
		{"no trunk at all", map[string]bool{}, "x", "", "", nil},
	}
	for _, c := range cases {
		if got := pickRanges(c.has, c.head, c.trunk, c.tag); !reflect.DeepEqual(got, c.want) {
			t.Errorf("%s:\n got %v\nwant %v", c.name, got, c.want)
		}
	}
}

func TestExactRefsIgnoresRefsBelowASlash(t *testing.T) {
	got := exactRefs([]byte("refs/heads/dev/x\nrefs/heads/main\n"), []string{refDev, refHead("main")})
	if !reflect.DeepEqual(got, map[string]bool{refHead("main"): true}) {
		t.Fatalf("got %v", got)
	}
}

func TestParseLog(t *testing.T) {
	out := "aaaa\x00aa\x00Fix: a thing\x00Pat\x001700000000\nbroken line\n"
	want := []commitRow{{SHA: "aaaa", Short: "aa", Subject: "Fix: a thing", Author: "Pat", Time: 1700000000000}}
	if got := parseLog([]byte(out)); !reflect.DeepEqual(got, want) {
		t.Fatalf("got %+v", got)
	}
}

func TestParseNumstatRenamesAndBinaries(t *testing.T) {
	out := "3\t1\tsrc/a.go\x00-\t-\tlogo.png\x001\t0\t\x00old name.md\x00new name.md\x00"
	want := []commitFile{
		{Path: "src/a.go", Added: 3, Removed: 1},
		{Path: "logo.png", Binary: true},
		{Path: "new name.md", From: "old name.md", Added: 1},
	}
	if got := parseNumstat([]byte(out)); !reflect.DeepEqual(got, want) {
		t.Fatalf("got %+v", got)
	}
}

func TestParsePatchKeepsHunksNotHeaders(t *testing.T) {
	patch := strings.Join([]string{
		"diff --git a/a.go b/a.go", "index 1..2 100644", "--- a/a.go", "+++ b/a.go",
		"@@ -3,2 +3,3 @@ func x() {", " keep", "-gone", "+came", "+also",
		"diff --git a/logo.png b/logo.png", "Binary files a/logo.png and b/logo.png differ",
		"diff --git a/b.go b/b.go", "--- a/b.go", "+++ b/b.go", "@@ -1 +1 @@", "-x", "\\ No newline at end of file", "+y", "",
	}, "\n")
	got := parsePatch([]byte(patch), false)
	want := []fileDiff{
		{Hunks: []diffHunk{{OldStart: 3, NewStart: 3, Lines: []string{" keep", "-gone", "+came", "+also"}}}},
		{Hunks: []diffHunk{}},
		{Hunks: []diffHunk{{OldStart: 1, NewStart: 1, Lines: []string{"-x", "\\ No newline at end of file", "+y"}}}},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %+v", got)
	}
}

func TestParsePatchCapsTheLines(t *testing.T) {
	lines := []string{"diff --git a/a b/a", "@@ -1 +1,4000 @@"}
	for range commitLines + 5 {
		lines = append(lines, "+x")
	}
	lines = append(lines, "diff --git a/b b/b", "@@ -1 +1 @@", "+y")
	got := parsePatch([]byte(strings.Join(lines, "\n")), false)
	if len(got) != 2 || len(got[0].Hunks[0].Lines) != commitLines || !got[0].Truncated || !got[1].Truncated || len(got[1].Hunks) != 0 {
		t.Fatalf("the cap is for the whole commit and marks what it cut: %d files, first %d lines %v, second %+v",
			len(got), len(got[0].Hunks[0].Lines), got[0].Truncated, got[1])
	}
	long := parsePatch([]byte("diff --git a/a b/a\n@@ -1 +1 @@\n+"+strings.Repeat("z", 5000)), true)
	if l := long[0].Hunks[0].Lines[0]; len(l) > commitLineChars+4 || !long[0].Truncated {
		t.Fatalf("a long line is cut, and a patch read in part marks its last file: %d %v", len(l), long[0].Truncated)
	}
	files := joinDiffs([]commitFile{{Path: "a"}, {Path: "b"}, {Path: "c.png", Binary: true}}, long)
	if !files[1].Diff.Truncated || files[2].Diff.Truncated || files[1].Diff.Hunks == nil {
		t.Fatalf("a file the patch did not reach is cut, a binary one has nothing to cut: %+v", files)
	}
}

// --- against real repos ---

type gitRepo struct {
	t   *testing.T
	dir string
}

func (g gitRepo) run(args ...string) string {
	g.t.Helper()
	cmd := exec.Command("git", append([]string{"-C", g.dir, "-c", "user.name=Pat", "-c", "user.email=pat@example.invalid", "-c", "init.defaultBranch=main"}, args...)...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		g.t.Fatalf("git %v: %v %s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func (g gitRepo) commit(name, text, msg string) string {
	g.t.Helper()
	if err := os.WriteFile(filepath.Join(g.dir, name), []byte(text), 0o644); err != nil {
		g.t.Fatal(err)
	}
	g.run("add", "-A")
	g.run("commit", "-q", "-m", msg)
	return g.run("rev-parse", "HEAD")
}

// newRepo is a repo with a bare origin it has pushed main to.
func newRepo(t *testing.T) gitRepo {
	root := t.TempDir()
	origin := gitRepo{t, filepath.Join(root, "origin.git")}
	g := gitRepo{t, filepath.Join(root, "work")}
	for _, d := range []string{origin.dir, g.dir} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	origin.run("init", "-q", "--bare")
	g.run("init", "-q")
	g.run("remote", "add", "origin", origin.dir)
	g.commit("a.txt", "one\n", "first")
	g.run("push", "-q", "origin", "main")
	return g
}

func subjects(g commitGroup) []string {
	var out []string
	for _, c := range g.Commits {
		out = append(out, c.Subject)
	}
	return out
}

func groupsOf(t *testing.T, dir string) map[string][]string {
	t.Helper()
	v, err := listCommits(context.Background(), dir)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string][]string{}
	for _, g := range v.Groups {
		got[g.Title] = subjects(g)
	}
	return got
}

func TestListCommitsInADevRepo(t *testing.T) {
	g := newRepo(t)
	g.run("checkout", "-q", "-b", "dev")
	g.commit("b.txt", "b\n", "pushed to dev")
	g.run("push", "-q", "origin", "dev")
	g.commit("c.txt", "c\n", "only here")
	want := map[string][]string{"Not pushed": {"only here"}, "Pushed, not shipped": {"pushed to dev"}}
	if got := groupsOf(t, g.dir); !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v", got)
	}
	g.run("checkout", "-q", "-b", "task/1")
	g.commit("d.txt", "d\n", "the task's")
	want["On this branch (task/1)"] = []string{"the task's"}
	if got := groupsOf(t, g.dir); !reflect.DeepEqual(got, want) {
		t.Fatalf("a task branch: got %v", got)
	}
}

func TestListCommitsInAMainOnlyRepo(t *testing.T) {
	g := newRepo(t)
	if got := groupsOf(t, g.dir); len(got) != 0 {
		t.Fatalf("no tag, all pushed: nothing waits, got %v", got)
	}
	g.run("tag", "v0.1.0")
	g.commit("b.txt", "b\n", "released? no")
	g.run("push", "-q", "origin", "main")
	g.commit("c.txt", "c\n", "local")
	want := map[string][]string{"Not pushed": {"local"}, "Since the last release": {"released? no"}}
	if got := groupsOf(t, g.dir); !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v", got)
	}
	g.run("checkout", "-q", "--detach")
	if got := groupsOf(t, g.dir); !reflect.DeepEqual(got, want) {
		t.Fatalf("detached: got %v", got)
	}
	v, _ := listCommits(context.Background(), g.dir)
	if v.Branch != "" || !v.Repo {
		t.Fatalf("detached is no branch: %+v", v)
	}
}

func TestListCommitsCapsAGroup(t *testing.T) {
	g := newRepo(t)
	for i := range commitsShown + 3 {
		g.run("commit", "-q", "--allow-empty", "-m", "c"+string(rune('a'+i%26)))
	}
	v, err := listCommits(context.Background(), g.dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(v.Groups) != 1 || len(v.Groups[0].Commits) != commitsShown || v.Groups[0].More != 3 {
		t.Fatalf("got %+v", v.Groups)
	}
	if c := v.Groups[0].Commits[0]; len(c.SHA) != 40 || c.Short == "" || c.Author != "Pat" || c.Time < 1e12 {
		t.Fatalf("a row: %+v", c)
	}
}

func TestReadCommitShowsMergesRenamesAndBinaries(t *testing.T) {
	g := newRepo(t)
	g.run("checkout", "-q", "-b", "side")
	g.run("mv", "a.txt", "moved.txt")
	if err := os.WriteFile(filepath.Join(g.dir, "logo.png"), []byte{0, 1, 2, 0, 255}, 0o644); err != nil {
		t.Fatal(err)
	}
	g.run("add", "-A")
	g.run("commit", "-q", "-m", "move and logo", "-m", "Why: tidy.")
	side := g.run("rev-parse", "HEAD")
	g.run("checkout", "-q", "main")
	g.commit("b.txt", "b\n", "on main")
	g.run("merge", "-q", "--no-ff", "-m", "merge side", "side")
	merge := g.run("rev-parse", "HEAD")

	d, err := readCommit(context.Background(), g.dir, side[:10])
	if err != nil {
		t.Fatal(err)
	}
	if d.SHA != side || d.Subject != "move and logo" || d.Body != "Why: tidy." || d.Author != "Pat" || len(d.Files) != 2 {
		t.Fatalf("got %+v", d)
	}
	byPath := map[string]commitFile{}
	for _, f := range d.Files {
		byPath[f.Path] = f
	}
	if f := byPath["moved.txt"]; f.From != "a.txt" {
		t.Errorf("a rename: %+v", f)
	}
	if f := byPath["logo.png"]; !f.Binary || len(f.Diff.Hunks) != 0 {
		t.Errorf("a binary file: %+v", f)
	}
	m, err := readCommit(context.Background(), g.dir, merge)
	if err != nil {
		t.Fatal(err)
	}
	if len(m.Files) != 2 {
		t.Fatalf("a merge is shown against its first parent: %+v", m.Files)
	}
	if _, err := readCommit(context.Background(), g.dir, "0000000"); !errors.Is(err, errNoCommit) {
		t.Fatalf("an unknown sha: %v", err)
	}
	tree := g.run("rev-parse", "HEAD^{tree}")
	if _, err := readCommit(context.Background(), g.dir, tree); !errors.Is(err, errNoCommit) {
		t.Fatalf("a tree is no commit: %v", err)
	}
}

func TestSessionCommitsRoutesAnswer(t *testing.T) {
	g := newRepo(t)
	sha := g.commit("b.txt", "one\ntwo\n", "local work")
	in := func(dir string) rowsFor { return rowsFor{sess: &store.RunnerSession{ID: "s1", Cwd: dir}} }
	cases := []struct {
		name, path string
		rows       rowsFor
		code       int
		says       string
	}{
		{"the list", "/v1/sessions/s1/commits", in(g.dir), http.StatusOK, `"title":"Not pushed"`},
		{"no repo", "/v1/sessions/s1/commits", in(t.TempDir()), http.StatusOK, `"repo":false`},
		{"folder gone", "/v1/sessions/s1/commits", in(filepath.Join(g.dir, "gone")), http.StatusBadGateway, "could not be read"},
		{"no session", "/v1/sessions/s1/commits", rowsFor{}, http.StatusNotFound, ""},
		{"one commit", "/v1/sessions/s1/commits/" + sha, in(g.dir), http.StatusOK, `"+two"`},
		{"upper case", "/v1/sessions/s1/commits/" + strings.ToUpper(sha), in(g.dir), http.StatusOK, `"subject":"local work"`},
		{"not hex", "/v1/sessions/s1/commits/--output=x", in(g.dir), http.StatusBadRequest, "hex"},
		{"too short", "/v1/sessions/s1/commits/abc", in(g.dir), http.StatusBadRequest, "hex"},
		{"unknown", "/v1/sessions/s1/commits/1234567", in(g.dir), http.StatusNotFound, "no such commit"},
		{"commit, no repo", "/v1/sessions/s1/commits/1234567", in(t.TempDir()), http.StatusNotFound, "not a git repo"},
	}
	for _, c := range cases {
		s := &Server{runners: &fakeRunners{}, runnerRows: c.rows}
		w := callSessions(t, s, "GET", c.path, "")
		if w.Code != c.code || !strings.Contains(w.Body.String(), c.says) {
			t.Errorf("%s: want %d %q, got %d %s", c.name, c.code, c.says, w.Code, w.Body)
		}
	}
	s := &Server{runners: &fakeRunners{}, runnerRows: in(t.TempDir())}
	var v commitsView
	if err := json.Unmarshal(callSessions(t, s, "GET", "/v1/sessions/s1/commits", "").Body.Bytes(), &v); err != nil || v.Groups == nil {
		t.Fatalf("no repo still has a list of groups, empty: %v %+v", err, v)
	}
}
