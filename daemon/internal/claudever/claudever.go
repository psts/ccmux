// Package claudever tells which Claude Code version is installed and which
// version each hosted pane is actually running. The native installer keeps
// every version as its own file under ~/.local/share/claude/versions/ and
// points ~/.local/bin/claude at the newest, so an update lands on disk while
// running sessions keep the old file open. A pane is behind when the file its
// Claude runs is older than the one the link names.
//
// Linux only: it reads /proc. Elsewhere Scan returns an error.
package claudever

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"ccmux.dev/ccmuxd/internal/listeners"
)

var versionRe = regexp.MustCompile(`^[0-9]+(\.[0-9]+)+$`)

// FromExe reads the version out of a Claude executable path, as readlink of
// /proc/<pid>/exe or of the launcher link gives it. The kernel appends
// " (deleted)" once the installer has removed an old version that is still
// running, which is the normal state of a pane that is behind.
func FromExe(target string) (string, bool) {
	target = strings.TrimSuffix(target, " (deleted)")
	if filepath.Base(filepath.Dir(target)) != "versions" {
		return "", false
	}
	v := filepath.Base(target)
	if !versionRe.MatchString(v) {
		return "", false
	}
	return v, true
}

// Installed returns the version the launcher link points at.
func Installed(link string) (string, error) {
	target, err := os.Readlink(link)
	if err != nil {
		return "", err
	}
	v, ok := FromExe(target)
	if !ok {
		return "", fmt.Errorf("%s points at %s, which names no version", link, target)
	}
	return v, nil
}

// Older reports whether version a is older than b, comparing dotted numbers.
// An empty or unparsable side is never older: unknown is not behind.
func Older(a, b string) bool {
	if !versionRe.MatchString(a) || !versionRe.MatchString(b) {
		return false
	}
	as, bs := strings.Split(a, "."), strings.Split(b, ".")
	for i := 0; i < len(as) || i < len(bs); i++ {
		x, y := part(as, i), part(bs, i)
		if x != y {
			return x < y
		}
	}
	return false
}

func part(parts []string, i int) int {
	if i >= len(parts) {
		return 0
	}
	n, _ := strconv.Atoi(parts[i])
	return n
}

// Proc is what a scan learned about one pane's foreground Claude.
type Proc struct {
	Version string
	// Argv is its command line, so a relaunch can check it still matches the
	// pane's startup command.
	Argv []string
	// Background reports a shell among its children. Claude runs every Bash
	// tool command as a child shell, so at an idle prompt one means a
	// background task (a dev server, a watch) that ending the session kills.
	Background bool
}

// shells are the programs a Bash tool command runs under.
var shells = map[string]bool{"bash": true, "zsh": true, "sh": true, "dash": true}

// proc is one row of a /proc walk.
type proc struct {
	pid, ppid int
	exe       string
}

// Scan returns pane id -> its foreground Claude for every pane it can see. A
// Claude with a Claude anywhere above it is that session's own work and is
// skipped: a subagent, or the ugrep the binary runs as under a Bash tool
// shell. When a pane holds two top-level Claudes the older wins, so the
// pane still reads as behind.
func Scan(root string) (map[string]Proc, error) {
	all, err := walk(root)
	if err != nil {
		return nil, err
	}
	byPID := make(map[int]proc, len(all))
	for _, p := range all {
		byPID[p.pid] = p
	}
	out := map[string]Proc{}
	for _, p := range all {
		v, ok := FromExe(p.exe)
		if !ok {
			continue
		}
		if underClaude(byPID, p.ppid) {
			continue
		}
		pane := paneOf(root, p.pid)
		if have, seen := out[pane]; pane == "" || seen && !Older(v, have.Version) {
			continue
		}
		out[pane] = Proc{Version: v, Argv: argvOf(root, p.pid), Background: hasShellChild(all, p.pid)}
	}
	return out, nil
}

// maxDepth bounds the walk up the parent chain; a pane's process tree is a
// handful deep, and a cycle in a racing read must not spin.
const maxDepth = 64

func underClaude(byPID map[int]proc, pid int) bool {
	for i := 0; i < maxDepth && pid > 1; i++ {
		p, ok := byPID[pid]
		if !ok {
			return false
		}
		if _, isClaude := FromExe(p.exe); isClaude {
			return true
		}
		pid = p.ppid
	}
	return false
}

func walk(root string) ([]proc, error) {
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil, err
	}
	var out []proc
	for _, e := range entries {
		pid, err := strconv.Atoi(e.Name())
		if err != nil {
			continue
		}
		exe, err := os.Readlink(filepath.Join(root, e.Name(), "exe"))
		if err != nil {
			continue // another user's process, or gone
		}
		out = append(out, proc{pid: pid, ppid: parentOf(root, pid), exe: exe})
	}
	return out, nil
}

func hasShellChild(all []proc, pid int) bool {
	for _, p := range all {
		if p.ppid == pid && shells[filepath.Base(strings.TrimSuffix(p.exe, " (deleted)"))] {
			return true
		}
	}
	return false
}

func argvOf(root string, pid int) []string {
	raw, err := os.ReadFile(filepath.Join(root, strconv.Itoa(pid), "cmdline"))
	if err != nil {
		return nil
	}
	return strings.Split(strings.TrimRight(string(raw), "\x00"), "\x00")
}

// parentOf is field 4 of /proc/<pid>/stat. 0 when unreadable.
func parentOf(root string, pid int) int {
	raw, err := os.ReadFile(filepath.Join(root, strconv.Itoa(pid), "stat"))
	if err != nil {
		return 0
	}
	// The comm field may hold spaces; everything after its closing paren is
	// well-formed.
	_, rest, ok := bytes.Cut(raw, []byte(") "))
	if !ok {
		return 0
	}
	f := strings.Fields(string(rest))
	if len(f) < 2 {
		return 0
	}
	n, _ := strconv.Atoi(f[1])
	return n
}

func paneOf(root string, pid int) string {
	raw, err := os.ReadFile(filepath.Join(root, strconv.Itoa(pid), "environ"))
	if err != nil {
		return ""
	}
	for _, kv := range bytes.Split(raw, []byte{0}) {
		if v, ok := bytes.CutPrefix(kv, []byte(listeners.EnvKey+"=")); ok {
			return string(v)
		}
	}
	return ""
}
