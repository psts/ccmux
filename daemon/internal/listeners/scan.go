// Package listeners discovers which TCP ports a hosted pane's processes are
// listening on, by reading the kernel's socket table and tying each listening
// socket back to the process that holds it and the CCMUX_PANE_ID that process
// inherited from its pane. This is how dev hostnames find their dev server
// without ever telling it where to bind: an app that pins `-p 3003` in its
// script, a monorepo runner starting three servers, or a uvicorn typed by
// hand all show up here on whatever port they chose.
//
// Linux only: it reads /proc. On a host without one, Scan returns an error and
// the caller falls back to routing hostnames to their configured port blindly.
package listeners

import (
	"bufio"
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

// Listener is one listening TCP port owned by a hosted pane's process.
type Listener struct {
	Port    int
	PID     int
	Process string // the kernel's short command name (/proc/<pid>/comm)
	PaneID  string
}

// EnvKey is the pane-identifying variable every hosted pane's shell carries;
// every process the shell starts inherits it.
const EnvKey = "CCMUX_PANE_ID"

// Scanner reads a proc tree. It caches which pane a pid belongs to, keyed by
// the process start time so a recycled pid is re-read.
type Scanner struct {
	root  string
	panes map[int]paneOf
}

type paneOf struct {
	start  string
	paneID string
}

// New returns a Scanner over the live /proc.
func New() *Scanner { return NewAt("/proc") }

// NewAt returns a Scanner over a proc tree rooted at root (tests).
func NewAt(root string) *Scanner { return &Scanner{root: root, panes: map[int]paneOf{}} }

// Scan returns every listening TCP port held by a pane process, one row per
// port, sorted by port. Sockets owned by processes outside any pane (system
// daemons, Docker's root-owned proxies) are not reported.
func (s *Scanner) Scan() ([]Listener, error) {
	byInode, err := s.listeningInodes()
	if err != nil {
		return nil, err
	}
	pids, err := s.pids()
	if err != nil {
		return nil, err
	}
	seen := map[int]bool{}
	var out []Listener
	for _, pid := range pids {
		paneID := s.paneFor(pid)
		if paneID == "" {
			continue
		}
		for _, inode := range s.socketInodes(pid) {
			port, ok := byInode[inode]
			if !ok || seen[port] {
				continue
			}
			seen[port] = true
			out = append(out, Listener{Port: port, PID: pid, Process: s.comm(pid), PaneID: paneID})
		}
	}
	s.forget(pids)
	sort.Slice(out, func(i, j int) bool { return out[i].Port < out[j].Port })
	return out, nil
}

// listeningInodes maps socket inode → port for every LISTEN row in net/tcp
// and net/tcp6.
func (s *Scanner) listeningInodes() (map[int]int, error) {
	out := map[int]int{}
	found := false
	for _, table := range []string{"tcp", "tcp6"} {
		f, err := os.Open(filepath.Join(s.root, "net", table))
		if err != nil {
			continue
		}
		found = true
		sc := bufio.NewScanner(f)
		sc.Scan() // header
		for sc.Scan() {
			if port, inode, ok := parseListenRow(sc.Text()); ok {
				out[inode] = port
			}
		}
		f.Close()
	}
	if !found {
		return nil, fmt.Errorf("no socket table under %s (not Linux?)", s.root)
	}
	return out, nil
}

// parseListenRow reads one net/tcp row: "sl local_address rem_address st ...
// inode". Only state 0A (LISTEN) counts.
func parseListenRow(line string) (port, inode int, ok bool) {
	f := strings.Fields(line)
	if len(f) < 10 || f[3] != "0A" {
		return 0, 0, false
	}
	_, portHex, found := strings.Cut(f[1], ":")
	if !found {
		return 0, 0, false
	}
	p, err := strconv.ParseInt(portHex, 16, 32)
	if err != nil {
		return 0, 0, false
	}
	ino, err := strconv.Atoi(f[9])
	if err != nil {
		return 0, 0, false
	}
	return int(p), ino, true
}

func (s *Scanner) pids() ([]int, error) {
	entries, err := os.ReadDir(s.root)
	if err != nil {
		return nil, err
	}
	var pids []int
	for _, e := range entries {
		if pid, err := strconv.Atoi(e.Name()); err == nil && e.IsDir() {
			pids = append(pids, pid)
		}
	}
	return pids, nil
}

// paneFor returns the pane id in a process's environment ("" = not a pane
// process, or unreadable — another user's process). Cached per (pid, start).
func (s *Scanner) paneFor(pid int) string {
	start := s.startTime(pid)
	if c, ok := s.panes[pid]; ok && c.start == start {
		return c.paneID
	}
	paneID := ""
	if raw, err := os.ReadFile(filepath.Join(s.root, strconv.Itoa(pid), "environ")); err == nil {
		for _, kv := range bytes.Split(raw, []byte{0}) {
			if v, ok := bytes.CutPrefix(kv, []byte(EnvKey+"=")); ok {
				paneID = string(v)
				break
			}
		}
	}
	s.panes[pid] = paneOf{start: start, paneID: paneID}
	return paneID
}

// startTime is field 22 of /proc/<pid>/stat, the clock tick the process
// started at — the cheapest thing that distinguishes a recycled pid.
func (s *Scanner) startTime(pid int) string {
	raw, err := os.ReadFile(filepath.Join(s.root, strconv.Itoa(pid), "stat"))
	if err != nil {
		return ""
	}
	// The comm field may hold spaces; everything after its closing paren is
	// well-formed.
	_, rest, ok := bytes.Cut(raw, []byte(") "))
	if !ok {
		return ""
	}
	f := strings.Fields(string(rest))
	if len(f) < 20 {
		return ""
	}
	return f[19]
}

func (s *Scanner) socketInodes(pid int) []int {
	fdDir := filepath.Join(s.root, strconv.Itoa(pid), "fd")
	entries, err := os.ReadDir(fdDir)
	if err != nil {
		return nil
	}
	var out []int
	for _, e := range entries {
		target, err := os.Readlink(filepath.Join(fdDir, e.Name()))
		if err != nil {
			continue
		}
		if n, ok := strings.CutPrefix(target, "socket:["); ok {
			if ino, err := strconv.Atoi(strings.TrimSuffix(n, "]")); err == nil {
				out = append(out, ino)
			}
		}
	}
	return out
}

func (s *Scanner) comm(pid int) string {
	raw, err := os.ReadFile(filepath.Join(s.root, strconv.Itoa(pid), "comm"))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(raw))
}

// forget drops cache entries for pids that no longer exist.
func (s *Scanner) forget(alive []int) {
	live := make(map[int]bool, len(alive))
	for _, pid := range alive {
		live[pid] = true
	}
	for pid := range s.panes {
		if !live[pid] {
			delete(s.panes, pid)
		}
	}
}
