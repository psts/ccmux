package claudever

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func TestFromExe(t *testing.T) {
	cases := []struct {
		in   string
		want string
		ok   bool
	}{
		{"/home/u/.local/share/claude/versions/2.1.301", "2.1.301", true},
		{"/home/u/.local/share/claude/versions/2.1.296 (deleted)", "2.1.296", true},
		{"/usr/bin/bash", "", false},
		{"/home/u/.local/share/claude/versions/latest", "", false},
		{"/x/node_modules/@anthropic-ai/claude-agent-sdk-linux-x64/claude", "", false},
	}
	for _, c := range cases {
		got, ok := FromExe(c.in)
		if got != c.want || ok != c.ok {
			t.Errorf("FromExe(%q) = %q,%v want %q,%v", c.in, got, ok, c.want, c.ok)
		}
	}
}

func TestOlder(t *testing.T) {
	cases := []struct {
		a, b string
		want bool
	}{
		{"2.1.296", "2.1.301", true},
		{"2.1.301", "2.1.301", false},
		{"2.1.301", "2.1.296", false},
		{"2.1.99", "2.1.100", true}, // numeric, not string order
		{"2.1", "2.1.1", true},
		{"", "2.1.1", false}, // unknown is never "behind"
		{"2.1.1", "", false},
	}
	for _, c := range cases {
		if got := Older(c.a, c.b); got != c.want {
			t.Errorf("Older(%q,%q) = %v want %v", c.a, c.b, got, c.want)
		}
	}
}

func TestInstalled(t *testing.T) {
	dir := t.TempDir()
	link := filepath.Join(dir, "claude")
	if err := os.Symlink("/home/u/.local/share/claude/versions/2.1.301", link); err != nil {
		t.Fatal(err)
	}
	if v, err := Installed(link); err != nil || v != "2.1.301" {
		t.Fatalf("Installed = %q, %v", v, err)
	}
	if _, err := Installed(filepath.Join(dir, "missing")); err == nil {
		t.Fatal("a missing link must be an error, not a version")
	}
	plain := filepath.Join(dir, "plain")
	if err := os.WriteFile(plain, []byte("#!/bin/sh"), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := Installed(plain); err == nil {
		t.Fatal("a link that names no version must be an error")
	}
}

// fakeProc writes one process into a fake proc tree.
func fakeProc(t *testing.T, root string, pid, ppid int, exe, pane string) {
	t.Helper()
	d := filepath.Join(root, strconv.Itoa(pid))
	if err := os.MkdirAll(d, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(exe, filepath.Join(d, "exe")); err != nil {
		t.Fatal(err)
	}
	stat := strconv.Itoa(pid) + " (2.1.x y) S " + strconv.Itoa(ppid) + " 1 1 0"
	if err := os.WriteFile(filepath.Join(d, "stat"), []byte(stat), 0o644); err != nil {
		t.Fatal(err)
	}
	env := "HOME=/home/u\x00"
	if pane != "" {
		env += "CCMUX_PANE_ID=" + pane + "\x00"
	}
	if err := os.WriteFile(filepath.Join(d, "environ"), []byte(env), 0o644); err != nil {
		t.Fatal(err)
	}
	argv := filepath.Base(exe) + "\x00--flag\x00value\x00"
	if err := os.WriteFile(filepath.Join(d, "cmdline"), []byte(argv), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestScan(t *testing.T) {
	root := t.TempDir()
	v := "/home/u/.local/share/claude/versions/"
	fakeProc(t, root, 10, 1, "/usr/bin/bash", "pane-a")
	fakeProc(t, root, 11, 10, v+"2.1.296 (deleted)", "pane-a")
	fakeProc(t, root, 12, 11, v+"2.1.296 (deleted)", "pane-a") // a child claude of the pane's claude
	fakeProc(t, root, 20, 1, "/usr/bin/bash", "pane-b")
	fakeProc(t, root, 21, 20, v+"2.1.301", "pane-b")
	fakeProc(t, root, 22, 21, "/usr/bin/bash", "pane-b")                  // a Bash tool command still running
	fakeProc(t, root, 23, 11, "/home/u/.local/bin/ccmux-peers", "pane-a") // an MCP server is not a task
	fakeProc(t, root, 24, 22, v+"2.1.301", "pane-b")                      // the binary run as ugrep under that shell
	fakeProc(t, root, 30, 1, v+"2.1.300", "")                             // outside any pane
	if err := os.WriteFile(filepath.Join(root, "self"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := Scan(root)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]Proc{
		"pane-a": {Version: "2.1.296", Argv: []string{"2.1.296 (deleted)", "--flag", "value"}},
		"pane-b": {Version: "2.1.301", Argv: []string{"2.1.301", "--flag", "value"}, Background: true},
	}
	if len(got) != len(want) {
		t.Fatalf("Scan = %v want %v", got, want)
	}
	for k, w := range want {
		g := got[k]
		if g.Version != w.Version || g.Background != w.Background || strings.Join(g.Argv, " ") != strings.Join(w.Argv, " ") {
			t.Errorf("pane %s: %+v want %+v", k, g, w)
		}
	}
}

func TestScanTwoClaudesInOnePaneKeepsTheOlder(t *testing.T) {
	root := t.TempDir()
	v := "/home/u/.local/share/claude/versions/"
	fakeProc(t, root, 10, 1, "/usr/bin/bash", "p")
	fakeProc(t, root, 11, 10, v+"2.1.301", "p")
	fakeProc(t, root, 13, 10, v+"2.1.296", "p")
	got, _ := Scan(root)
	if got["p"].Version != "2.1.296" {
		t.Fatalf("got %q, want the older version so the pane still reads behind", got["p"].Version)
	}
}
