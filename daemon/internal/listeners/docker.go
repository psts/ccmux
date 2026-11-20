package listeners

import (
	"bytes"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// A dev server run in a container ("docker run -p 8090:80 site") listens
// through Docker's own proxy, a root process outside every pane, so the
// socket scan finds no owner for its port. The `docker run` client is the
// pane's, though: it carries the pane id and names the host port. Its
// published ports count as the pane's when they are really listening.
// Seen live 2026-11-20 (sanlabs-web): the Hostnames dialog said nothing
// listens while the site answered on 8090.

// containerRunners are the clients whose `run -p` is read.
var containerRunners = map[string]bool{"docker": true, "podman": true}

// published is the host ports a container client's command line publishes:
// `run` with -p/--publish host:container (an address and a /proto may be
// on either side). A port Docker picks itself (-P, "::80", "80") and a
// range are left out: nothing in the command says which port it is.
func published(args []string) []int {
	if !isRun(args) {
		return nil
	}
	var out []int
	for i := 1; i < len(args); i++ {
		spec, next := publishSpec(args, i)
		if spec == "" {
			continue
		}
		i = next
		if port := hostPort(spec); port > 0 {
			out = append(out, port)
		}
	}
	return out
}

// isRun reports whether a client command line runs a container: its first
// word that is not a flag is "run" (docker run, docker container run).
func isRun(args []string) bool {
	for _, a := range args[1:] {
		if strings.HasPrefix(a, "-") || a == "container" {
			continue
		}
		return a == "run"
	}
	return false
}

// publishSpec is the value of a publish flag at args[i] and the index of
// the last word it used; "" when args[i] is no publish flag.
func publishSpec(args []string, i int) (string, int) {
	a := args[i]
	switch {
	case a == "-p" || a == "--publish":
		if i+1 < len(args) {
			return args[i+1], i + 1
		}
	case strings.HasPrefix(a, "--publish="):
		return strings.TrimPrefix(a, "--publish="), i
	case strings.HasPrefix(a, "-p") && len(a) > 2 && a[2] != '-':
		return a[2:], i
	}
	return "", i
}

// hostPort is the host side of "[addr:]host:container[/proto]", 0 when
// there is none to read.
func hostPort(spec string) int {
	spec, _, _ = strings.Cut(spec, "/")
	left, _, ok := cutLast(spec, ":")
	if !ok {
		return 0 // "80": Docker picks the host port
	}
	if _, after, ok := cutLast(left, ":"); ok {
		left = after // "127.0.0.1:8090" or "[::1]:8090"
	}
	port, err := strconv.Atoi(left)
	if err != nil || port < 1 || port > 65535 {
		return 0 // "", a range, or not a number
	}
	return port
}

func cutLast(s, sep string) (string, string, bool) {
	i := strings.LastIndex(s, sep)
	if i < 0 {
		return s, "", false
	}
	return s[:i], s[i+len(sep):], true
}

// cmdline is a process's arguments.
func (s *Scanner) cmdline(pid int) []string {
	raw, err := os.ReadFile(filepath.Join(s.root, strconv.Itoa(pid), "cmdline"))
	if err != nil || len(raw) == 0 {
		return nil
	}
	var out []string
	for _, a := range bytes.Split(bytes.TrimRight(raw, "\x00"), []byte{0}) {
		out = append(out, string(a))
	}
	return out
}

// containerPorts is the listening host ports a pane's container client
// publishes, as rows for that pane.
func (s *Scanner) containerPorts(pid int, paneID string, listening map[int]bool) []Listener {
	comm := s.comm(pid)
	if !containerRunners[comm] {
		return nil
	}
	var out []Listener
	for _, port := range published(s.cmdline(pid)) {
		if listening[port] {
			out = append(out, Listener{Port: port, PID: pid, Process: comm, PaneID: paneID})
		}
	}
	return out
}
