package api

import (
	"context"
	"errors"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"ccmux.dev/ccmuxd/internal/store"
)

func TestMatchFilesRanksFileNamesBeforePaths(t *testing.T) {
	paths := []string{"src/components/Header.tsx", "src/head.go", "docs/overhead.md", "README.md", "src/hello/main.go", "web/x/h_e_a_d.txt"}
	cases := []struct {
		q    string
		n    int
		want []string
	}{
		// name starts with it, name holds it, letters in order; case aside
		{"head", 10, []string{"src/head.go", "src/components/Header.tsx", "docs/overhead.md", "web/x/h_e_a_d.txt"}},
		{"HEAD", 10, []string{"src/head.go", "src/components/Header.tsx", "docs/overhead.md", "web/x/h_e_a_d.txt"}},
		{"hello/", 10, []string{"src/hello/main.go"}}, // a folder in the path
		{"zzz", 10, []string{}},
		{"", 3, []string{"README.md", "src/head.go", "docs/overhead.md"}}, // shallowest first
		{"head", 1, []string{"src/head.go"}},                              // capped
	}
	for _, c := range cases {
		if got := matchFiles(paths, c.q, c.n); !slices.Equal(got, c.want) {
			t.Errorf("%q: got %v, want %v", c.q, got, c.want)
		}
	}
}

// gitFiles lists what git tracks and what it would track, never what it
// ignores, and says a folder that is no repo is one.
func TestGitFilesListsTrackedAndNewButNotIgnored(t *testing.T) {
	dir := t.TempDir()
	run := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
	}
	run("init", "-q")
	write := func(name, text string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(filepath.Join(dir, name)), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, name), []byte(text), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write(".gitignore", "secret.env\n")
	write("src/tracked.go", "package x\n")
	run("add", ".gitignore", "src/tracked.go")
	write("new file.md", "untracked, with a space\n")
	write("secret.env", "KEY=1\n")
	got, err := gitFiles(context.Background(), dir)
	if err != nil {
		t.Fatal(err)
	}
	slices.Sort(got)
	if want := []string{".gitignore", "new file.md", "src/tracked.go"}; !slices.Equal(got, want) {
		t.Fatalf("got %q, want %q", got, want)
	}
	if _, err := gitFiles(context.Background(), t.TempDir()); !errors.Is(err, errNotRepo) {
		t.Fatalf("a folder that is no repo: %v", err)
	}
}

// Only git's own "not a repository" is a quiet empty list: a folder that
// is gone and a git that ran out of time are failures, with a reason.
func TestGitFilesTellsNotARepoFromAFailure(t *testing.T) {
	if _, err := gitFiles(context.Background(), filepath.Join(t.TempDir(), "gone")); err == nil || errors.Is(err, errNotRepo) || !strings.Contains(err.Error(), "cannot change to") {
		t.Fatalf("a folder that is gone is a failure, with git's reason: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := gitFiles(ctx, t.TempDir()); !errors.Is(err, context.Canceled) {
		t.Fatalf("out of time is said as such, not as not-a-repo: %v", err)
	}
}

// rowsFor stands in for the session rows: one session in dir, or a read
// that fails.
type rowsFor struct {
	runnerRows
	sess *store.RunnerSession
	err  error
}

func (r rowsFor) RunnerSession(string) (*store.RunnerSession, error) { return r.sess, r.err }

func TestSessionFilesRouteAnswers(t *testing.T) {
	repo := t.TempDir()
	if out, err := exec.Command("git", "-C", repo, "init", "-q").CombinedOutput(); err != nil {
		t.Fatalf("%v %s", err, out)
	}
	if err := os.WriteFile(filepath.Join(repo, "head.go"), []byte("package x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name string
		rows rowsFor
		code int
		says string
	}{
		{"a repo", rowsFor{sess: &store.RunnerSession{ID: "s1", Cwd: repo}}, http.StatusOK, `"head.go"`},
		{"no repo", rowsFor{sess: &store.RunnerSession{ID: "s1", Cwd: t.TempDir()}}, http.StatusOK, "not a git repo"},
		{"folder gone", rowsFor{sess: &store.RunnerSession{ID: "s1", Cwd: filepath.Join(repo, "gone")}}, http.StatusBadGateway, "could not be listed"},
		{"no session", rowsFor{}, http.StatusNotFound, ""},
		{"rows unread", rowsFor{err: errors.New("database is locked")}, http.StatusInternalServerError, "database is locked"},
	}
	for _, c := range cases {
		s := &Server{runners: &fakeRunners{}, runnerRows: c.rows}
		w := callSessions(t, s, "GET", "/v1/sessions/s1/files?q=he", "")
		if w.Code != c.code || !strings.Contains(w.Body.String(), c.says) {
			t.Errorf("%s: want %d %q, got %d %s", c.name, c.code, c.says, w.Code, w.Body)
		}
	}
}
