// Package walls contains a unit's work: the caps every session and job
// service runs under, and, when the Sandbox setting is on, the walls around
// one unit (a read-only system and home, write access to its own folders
// only, a private /tmp), as a systemd drop-in file for that one unit.
//
// Walls are systemd's to build and this package's to prove. On a host that
// does not allow them (an AppArmor rule against unprivileged user
// namespaces, as Ubuntu ships it) systemd starts the unit anyway, fully
// open, and says nothing. So a walled unit's own process checks the walls
// before it does any work (Check), and refuses to run when they are not
// there.
//
// It owns no work and imports nothing of the daemon's: the jobs and the
// session runners both use it.
package walls

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Plan is the walls of one unit.
type Plan struct {
	// Write is every folder the unit may write. Each must exist: systemd
	// fails a unit whose writable path is missing, before any of its code
	// runs.
	Write []string
	// Caches are folders it may write when they exist (build caches, Claude
	// Code's own folder): a host without one of them runs without it.
	Caches []string
	// Sealed are paths inside a writable folder that stay read-only: what
	// would run later, outside the walls, if the unit could write it (a
	// repo's git hooks and its git config).
	Sealed []string
	// Outside are paths the unit must not be able to write, which the check
	// tries: the proof that the walls are up. They are named in no unit
	// file line.
	Outside []string
	// Hidden are sockets the unit must not reach at all, when they are
	// there: the user's bus and systemd's own socket. Through them a unit
	// asks the user's systemd to run a command, which then runs outside
	// every wall (seen live: `systemd-run --user` from inside one).
	Hidden []string
	// NoNetwork gives the unit a network of its own with nothing in it but
	// loopback.
	NoNetwork bool
	// ShellBox asks for Claude Code's own sandbox around a session's shell
	// commands, inside the same folders (Host.ClaudeSandbox says whether
	// the host can). It changes nothing in the unit file.
	ShellBox bool
}

// Open is every folder the plan opens that is there now: what a second
// wall inside the first (Claude Code's own sandbox) is told to open too.
func (p Plan) Open() []string {
	return append(clean(p.Write), existing(clean(p.Caches)...)...)
}

// dropInHead opens every walls drop-in: a comment to systemd, and how a
// person who finds the file learns whose it is.
const dropInHead = "# Written by ccmuxd (the Sandbox setting): this unit's walls. Do not edit: it is rewritten at every start.\n"

// families are the kinds of socket a walled unit may open: the internet,
// unix sockets, and netlink (which Go and Node read the network interfaces
// through). Everything else (raw packets, Bluetooth, ...) is refused.
const families = "AF_UNIX AF_INET AF_INET6 AF_NETLINK"

// DropIn is the unit file text of the plan: a drop-in for one instance of a
// unit template. ProtectSystem=strict leaves /home, /root and /run/user
// writable (seen live on systemd 255), so ProtectHome=read-only is what
// closes the home folder.
func (p Plan) DropIn() string {
	var b strings.Builder
	b.WriteString(dropInHead)
	b.WriteString("[Service]\nProtectSystem=strict\nProtectHome=read-only\nPrivateTmp=yes\n")
	b.WriteString("RestrictAddressFamilies=" + families + "\n")
	var rw []string
	for _, path := range clean(p.Write) {
		rw = append(rw, Word(path))
	}
	for _, path := range clean(p.Caches) {
		rw = append(rw, Word("-"+path))
	}
	if len(rw) > 0 {
		b.WriteString("ReadWritePaths=" + strings.Join(rw, " ") + "\n")
	}
	var ro []string
	for _, path := range clean(p.Sealed) {
		ro = append(ro, Word(path))
	}
	if len(ro) > 0 {
		b.WriteString("ReadOnlyPaths=" + strings.Join(ro, " ") + "\n")
	}
	var hidden []string
	for _, path := range clean(p.Hidden) {
		hidden = append(hidden, Word("-"+path))
	}
	if len(hidden) > 0 {
		b.WriteString("InaccessiblePaths=" + strings.Join(hidden, " ") + "\n")
	}
	if p.NoNetwork {
		b.WriteString("PrivateNetwork=yes\n")
	}
	return b.String()
}

// Validate refuses a plan systemd would fail on, or would read as something
// else: a path that is not absolute, one with a line break in it (it would
// start a new line of the unit file), one under /tmp or /var/tmp (the
// unit's own /tmp is private, so that folder is not there inside it), and
// a folder the unit must find that is not there. systemd fails such a unit
// before any of its code runs, with a status (226) that names no path; the
// error here does.
func (p Plan) Validate() error {
	for _, list := range [][]string{p.Write, p.Caches, p.Sealed, p.Hidden} {
		for _, path := range list {
			if err := ValidPath(path); err != nil {
				return err
			}
		}
	}
	for _, list := range [][]string{p.Write, p.Sealed} {
		for _, path := range list {
			if _, err := os.Stat(path); err != nil {
				return fmt.Errorf("%s, which its walls name, is not there: %w", path, err)
			}
		}
	}
	return nil
}

// ValidPath says why a path cannot be named in a unit's walls, nil when it
// can.
func ValidPath(path string) error {
	switch {
	case !filepath.IsAbs(path):
		return fmt.Errorf("%q is not a full path", path)
	case strings.ContainsAny(path, "\n\r"):
		return errors.New("a path with a line break in it cannot be walled")
	}
	for _, tmp := range PrivateTemp {
		if under(path, tmp) {
			return fmt.Errorf("%s is under a temporary folder, and a walled unit has a /tmp of its own: move it elsewhere", path)
		}
	}
	return nil
}

// PrivateTemp are the folders a walled unit gets an empty copy of
// (PrivateTmp=yes), so nothing under them can be named in its walls. A
// test whose own folders live under one of them empties the list: it
// starts no real unit.
var PrivateTemp = []string{"/tmp", "/var/tmp"}

// under reports whether path is dir or inside it.
func under(path, dir string) bool {
	path = filepath.Clean(path)
	return path == dir || strings.HasPrefix(path, dir+string(filepath.Separator))
}

// clean is the paths cleaned, each once, in a fixed order: the same plan is
// the same text, so an unchanged drop-in is not written again.
func clean(paths []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, path := range paths {
		if path == "" {
			continue
		}
		path = filepath.Clean(path)
		if !seen[path] {
			seen[path] = true
			out = append(out, path)
		}
	}
	sort.Strings(out)
	return out
}

// Word quotes one word of a unit file line: "%" is systemd's specifier
// sign, and a word with a space or a quote in it needs quoting.
func Word(s string) string {
	s = strings.ReplaceAll(s, "%", "%%")
	if !strings.ContainsAny(s, " \t\"'\\") {
		return s
	}
	s = strings.ReplaceAll(s, `\`, `\\`)
	return `"` + strings.ReplaceAll(s, `"`, `\"`) + `"`
}

// Expect is what a walled unit's own process checks before it does any
// work. It rides in the unit's spec file.
type Expect struct {
	// HostNS is the mount namespace of the daemon that wrote the spec, as
	// /proc/self/ns/mnt names it: a walled unit is in another one.
	HostNS string `json:"hostNs"`
	// Writable must take a write, Refused must refuse one as a read-only
	// file system.
	Writable []string `json:"writable,omitempty"`
	Refused  []string `json:"refused,omitempty"`
	// Hidden must not be reachable: a socket there takes no connection.
	Hidden []string `json:"hidden,omitempty"`
	// NoNetwork: nothing but loopback may be there.
	NoNetwork bool `json:"noNetwork,omitempty"`
}

// Expect is what a unit under this plan checks, written by a daemon whose
// mount namespace is hostNS.
func (p Plan) Expect(hostNS string) *Expect {
	refused := append(clean(p.Sealed), clean(p.Outside)...)
	return &Expect{HostNS: hostNS, Writable: clean(p.Write), Refused: refused, Hidden: clean(p.Hidden), NoNetwork: p.NoNetwork}
}
