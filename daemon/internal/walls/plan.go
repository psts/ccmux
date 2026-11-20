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
	// Hidden are paths the unit must not reach at all, when they are there.
	// Sockets: the user's bus and systemd's own socket, through which a unit
	// asks the user's systemd to run a command, which then runs outside
	// every wall (seen live: `systemd-run --user` from inside one). Files
	// and folders: what opens a door outside the walls to whoever reads it
	// (the daemon's API token, the bus's secret, the registry).
	//
	// Hide a folder rather than a file the daemon writes again: a file that
	// is replaced by a rename is a new file, which a unit that was already
	// running then sees.
	Hidden []string
	// Own are folders of the daemon's of which the unit sees only what is
	// its own (the sessions' specs, their sockets, the jobs' folders).
	Own []Own
	// Private are folders the unit gets an empty one of its own in place
	// of, kept under PrivateRoot: what Claude Code writes that another
	// session then runs (claude.go). Each must exist.
	Private []string
	// PrivateRoot is a folder of the unit's own, outside the walls' reach
	// of any other unit, that holds its private folders.
	PrivateRoot string
	// Apart are folders the unit sees another folder in place of, and may
	// write there: the caches, which walled units share with each other and
	// with nothing outside the walls (git.go, ApartCaches).
	Apart []Apart
	// NoNetwork gives the unit a network of its own with nothing in it but
	// loopback.
	NoNetwork bool
	// ShellBox asks for Claude Code's own sandbox around a session's shell
	// commands, inside the same folders (Host.ClaudeSandbox says whether
	// the host can). It changes nothing in the unit file.
	ShellBox bool
}

// Own is a folder shared by many units, as one of them sees it: empty and
// read-only, but for what is its own.
type Own struct {
	// Dir is the shared folder. It must exist.
	Dir string
	// Write are the folders in Dir the unit keeps and may write. Each must
	// exist.
	Write []string
	// Read are the files and folders in Dir the unit keeps and may read. One
	// that is missing is left out.
	Read []string
}

// Apart is a folder the unit sees in another's place: From is mounted over
// To. Both must exist.
type Apart struct{ From, To string }

// kept is the names in Dir the unit may see.
func (o Own) kept() []string {
	var names []string
	for _, path := range append(clean(o.Write), clean(o.Read)...) {
		if rel, err := filepath.Rel(filepath.Clean(o.Dir), path); err == nil {
			names = append(names, strings.Split(rel, string(filepath.Separator))[0])
		}
	}
	return names
}

// Open is every folder the plan opens that is there now: what a second
// wall inside the first (Claude Code's own sandbox) is told to open too.
func (p Plan) Open() []string {
	return append(p.writable(), existing(clean(p.Caches)...)...)
}

// writable is every folder the unit must be able to write: what the plan
// opens, what it keeps of the shared folders, and its private ones.
func (p Plan) writable() []string {
	out := append([]string(nil), p.Write...)
	for _, o := range p.Own {
		out = append(out, o.Write...)
	}
	for _, a := range p.Apart {
		out = append(out, a.To)
	}
	return clean(append(out, p.Private...))
}

// privateDir is the folder of the unit's own that stands in for target.
func (p Plan) privateDir(target string) string {
	flat := strings.ReplaceAll(strings.Trim(filepath.Clean(target), string(filepath.Separator)), string(filepath.Separator), "_")
	return filepath.Join(p.PrivateRoot, "private", flat)
}

// MakePrivate makes the unit's own folders that stand in for its private
// ones: systemd wants each to be there before the unit starts.
func (p Plan) MakePrivate() error {
	for _, target := range clean(p.Private) {
		if err := os.MkdirAll(p.privateDir(target), 0o700); err != nil {
			return fmt.Errorf("a private folder for %s: %w", target, err)
		}
	}
	return nil
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
	b.WriteString(p.mounts())
	if p.NoNetwork {
		b.WriteString("PrivateNetwork=yes\n")
	}
	return b.String()
}

// mounts are the lines that give the unit its own view of a folder: a
// shared folder emptied but for what is the unit's own, and a private
// folder in place of one every session shares.
func (p Plan) mounts() string {
	var empty, rw, ro []string
	for _, o := range p.Own {
		empty = append(empty, Word(filepath.Clean(o.Dir)+":ro"))
		rw = append(rw, o.Write...)
		ro = append(ro, o.Read...)
	}
	sort.Strings(empty)
	var b strings.Builder
	line := func(key string, words []string) {
		if len(words) > 0 {
			b.WriteString(key + "=" + strings.Join(words, " ") + "\n")
		}
	}
	line("TemporaryFileSystem", empty)
	var binds []string
	for _, path := range clean(rw) {
		binds = append(binds, Word(path))
	}
	for _, target := range clean(p.Private) {
		binds = append(binds, Word(p.privateDir(target)+":"+target))
	}
	binds = append(binds, p.apartBinds()...)
	line("BindPaths", binds)
	var reads []string
	for _, path := range clean(ro) {
		reads = append(reads, Word("-"+path))
	}
	line("BindReadOnlyPaths", reads)
	return b.String()
}

// apartBinds are the words that mount each folder that stands apart, in a
// fixed order.
func (p Plan) apartBinds() []string {
	var out []string
	for _, a := range p.Apart {
		out = append(out, Word(filepath.Clean(a.From)+":"+filepath.Clean(a.To)))
	}
	sort.Strings(out)
	return out
}

// Validate refuses a plan systemd would fail on, or would read as something
// else: a path that is not absolute, one with a line break in it (it would
// start a new line of the unit file), one under /tmp or /var/tmp (the
// unit's own /tmp is private, so that folder is not there inside it), and
// a folder the unit must find that is not there. systemd fails such a unit
// before any of its code runs, with a status (226) that names no path; the
// error here does.
func (p Plan) Validate() error {
	must, may, err := p.named()
	if err != nil {
		return err
	}
	for _, path := range append(must, may...) {
		if err := ValidPath(path); err != nil {
			return err
		}
	}
	for _, path := range must {
		if _, err := os.Stat(path); err != nil {
			return fmt.Errorf("%s, which its walls name, is not there: %w", path, err)
		}
	}
	return nil
}

// named is every path the plan names in the unit file: the ones the unit
// must find, and the ones that may be missing. A plan that does not hold
// together (what a unit keeps of a shared folder is not in it, private
// folders with nowhere to keep them) is an error.
func (p Plan) named() (must, may []string, err error) {
	must = append(append(must, p.Write...), p.Sealed...)
	may = append(append(may, p.Caches...), p.Hidden...)
	for _, o := range p.Own {
		must = append(append(must, o.Dir), o.Write...)
		may = append(may, o.Read...)
		for _, path := range append(clean(o.Write), clean(o.Read)...) {
			if path == filepath.Clean(o.Dir) || !under(path, filepath.Clean(o.Dir)) {
				return nil, nil, fmt.Errorf("%s is not inside %s, of which it is the unit's own part", path, o.Dir)
			}
		}
	}
	for _, a := range p.Apart {
		must = append(must, a.From, a.To)
	}
	if len(p.Private) == 0 {
		return must, may, nil
	}
	if p.PrivateRoot == "" {
		return nil, nil, errors.New("the plan has private folders and no folder of the unit's own to keep them in")
	}
	must = append(must, p.Private...)
	for _, target := range clean(p.Private) {
		must = append(must, p.privateDir(target))
	}
	return must, may, nil
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
	// Hidden must not be reachable: a socket there takes no connection, a
	// file cannot be opened, a folder cannot be listed.
	Hidden []string `json:"hidden,omitempty"`
	// Only names, for a folder many units share, the entries this unit may
	// see in it: anything else there is another unit's, and the walls are
	// not up.
	Only map[string][]string `json:"only,omitempty"`
	// NoNetwork: nothing but loopback may be there.
	NoNetwork bool `json:"noNetwork,omitempty"`
}

// Expect is what a unit under this plan checks, written by a daemon whose
// mount namespace is hostNS.
func (p Plan) Expect(hostNS string) *Expect {
	refused := append(clean(p.Sealed), clean(p.Outside)...)
	e := &Expect{HostNS: hostNS, Writable: p.writable(), Refused: refused, Hidden: clean(p.Hidden), NoNetwork: p.NoNetwork}
	for _, o := range p.Own {
		if e.Only == nil {
			e.Only = map[string][]string{}
		}
		e.Only[filepath.Clean(o.Dir)] = o.kept()
	}
	return e
}
