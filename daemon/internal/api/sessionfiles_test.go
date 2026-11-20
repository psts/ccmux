package api

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"testing"
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
