package walls

import (
	"bytes"
	"errors"
	"fmt"
	"net"
	"os"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// ExitRefused is the exit status of a unit that refused to run because its
// walls are not up. It is a status of its own so systemd does not start a
// session again on it (the unit template's RestartPreventExitStatus) and
// the daemon can tell it from a crash.
const ExitRefused = 97

// ErrNoWalls is a walled unit that is not behind its walls. The text after
// it says what the check found.
var ErrNoWalls = errors.New("the sandbox is on, and this unit's walls are not up")

// probes are how the check asks the system; a test gives its own.
type probes struct {
	mountNS    func() (string, error)
	tryWrite   func(path string) error
	canWrite   func(path string) error
	reach      func(path string) error
	list       func(dir string) ([]string, error)
	outside    func() error
	interfaces func() ([]net.Interface, error)
}

var system = probes{mountNS: MountNS, tryWrite: tryWrite, canWrite: canWrite, reach: reach, list: list, outside: outside, interfaces: net.Interfaces}

// Check proves the walls are up around this process, and says why not
// otherwise (an ErrNoWalls). A unit calls it first thing and does no work
// on an error: a unit that is meant to be walled never runs open.
//
// These must hold. The process is in a mount namespace of its own (not
// the daemon's). Every path it must not write refuses a real write as a
// read-only file system: "permission denied" would not do, since that is
// the file's own mode and proves no wall. Every folder it is meant to
// write takes one, or its work would fail later with an error that names
// no wall. What is hidden from it cannot be reached: a socket takes no
// connection, a file does not open, a folder does not list. A folder it
// shares with other units shows only what is its own. And no process
// outside the walls can be walked into through /proc.
func Check(e Expect) error { return system.check(e) }

func (p probes) check(e Expect) error {
	if err := p.ownNamespace(e); err != nil {
		return err
	}
	if len(e.Refused) == 0 {
		return noWalls("its spec names nothing that must refuse a write, so nothing proves a wall")
	}
	for _, path := range e.Refused {
		if err := p.mustRefuse(path); err != nil {
			return err
		}
	}
	for _, path := range e.Writable {
		if err := p.canWrite(path); err != nil {
			return noWalls(fmt.Sprintf("%s, which its work writes, cannot be written: %v", path, err))
		}
	}
	if err := p.mustHide(e); err != nil {
		return err
	}
	if e.NoNetwork {
		return p.mustBeOffline()
	}
	return nil
}

// mustHide holds when nothing that is hidden from the unit can be reached:
// the hidden paths, the other units' part of a shared folder, and the
// processes outside the walls.
func (p probes) mustHide(e Expect) error {
	for _, path := range e.Hidden {
		// A real try, not a question about rights: a socket on a read-only
		// mount reads as unwritable and still takes a connection. A path that
		// is not there at all is as good as hidden.
		if err := p.reach(path); err == nil {
			return noWalls(path + " can be reached, and what is there opens a way out of the walls")
		}
	}
	for dir, own := range e.Only {
		names, err := p.list(dir)
		if err != nil {
			return noWalls(fmt.Sprintf("%s, of which it is to see its own part only, cannot be listed: %v", dir, err))
		}
		for _, name := range names {
			if !slices.Contains(own, name) {
				return noWalls(fmt.Sprintf("%s shows %s, which is another unit's", dir, name))
			}
		}
	}
	if err := p.outside(); err != nil {
		return noWalls(err.Error())
	}
	return nil
}

// ownNamespace holds when this process is in another mount namespace than
// the daemon that wrote its spec.
func (p probes) ownNamespace(e Expect) error {
	if e.HostNS == "" {
		return noWalls("its spec does not name the daemon's mount namespace, so there is nothing to tell its own from")
	}
	own, err := p.mountNS()
	if err != nil {
		return noWalls("its mount namespace cannot be read: " + err.Error())
	}
	if own == e.HostNS {
		return noWalls("it runs in the daemon's own mount namespace (" + own + "): systemd built no walls")
	}
	return nil
}

// mustRefuse tries a write at path, which must fail as a read-only file
// system.
func (p probes) mustRefuse(path string) error {
	err := p.tryWrite(path)
	switch {
	case err == nil:
		return noWalls(path + " took a write, and it must be read-only")
	case errors.Is(err, syscall.EROFS):
		return nil
	}
	return noWalls(fmt.Sprintf("%s refused a write, but not as a read-only file system (%v), which proves no wall", path, err))
}

// mustBeOffline holds when nothing but loopback is there.
func (p probes) mustBeOffline() error {
	list, err := p.interfaces()
	if err != nil {
		return noWalls("its network interfaces cannot be read: " + err.Error())
	}
	for _, i := range list {
		if i.Flags&net.FlagLoopback == 0 {
			return noWalls("it was to have no network, and " + i.Name + " is there")
		}
	}
	return nil
}

func noWalls(what string) error { return fmt.Errorf("%w: %s", ErrNoWalls, what) }

// MountNS names the mount namespace this process is in, as the kernel does
// ("mnt:[4026531832]").
func MountNS() (string, error) { return os.Readlink("/proc/self/ns/mnt") }

// tryWrite really tries to write at path and undoes it at once when it
// worked: a file is opened for writing and nothing is written, a folder
// gets a file that is removed again. On a walled path neither leaves a
// trace; on an open one the trace is gone before this returns.
func tryWrite(path string) error {
	fi, err := os.Stat(path)
	if err != nil {
		return err
	}
	if !fi.IsDir() {
		f, err := os.OpenFile(path, os.O_WRONLY, 0)
		if err != nil {
			return err
		}
		return f.Close()
	}
	f, err := os.CreateTemp(path, ".ccmux-walls-probe-*")
	if err != nil {
		return err
	}
	f.Close()
	return os.Remove(f.Name())
}

// canWrite asks the kernel whether path would take a write, and writes
// nothing: a folder inside a repo's .git must not get a stray file even
// for a moment, since git would read it as a ref.
func canWrite(path string) error { return syscall.Access(path, 2) }

// reach tries to get at what is at path, and says why it could not: a
// socket is connected to (and the connection closed at once, with nothing
// said on it), a folder is listed, a file is opened for reading.
func reach(path string) error {
	fi, err := os.Stat(path)
	if err != nil {
		return err
	}
	switch {
	case fi.Mode()&os.ModeSocket != 0:
		c, err := net.DialTimeout("unix", path, 2*time.Second)
		if err != nil {
			return err
		}
		return c.Close()
	case fi.IsDir():
		_, err := os.ReadDir(path)
		return err
	}
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	return f.Close()
}

// list is the names in a folder.
func list(dir string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	names := make([]string, len(entries))
	for i, e := range entries {
		names[i] = e.Name()
	}
	return names, nil
}

// outside reports a process outside the walls that can be walked into
// through /proc: its root folder there is the whole system as that process
// sees it, with no wall in it (/proc/<pid>/root/home/...), and its
// environment may hold what the walls hide. It is nil when the walls hold.
//
// The process to try is the nearest ancestor in another mount namespace,
// which for a unit's own process is the user's systemd. systemd builds a
// user unit's walls inside a user namespace of its own, and from there the
// kernel refuses that walk (seen live 2026-11-20: permission denied on its
// root, its environment and its namespaces). A host where the walls came
// up some other way would not refuse it, and nothing else here would tell.
func outside() error {
	own, err := MountNS()
	if err != nil {
		return nil // check() has asked already, and said so
	}
	pid := os.Getppid()
	for hops := 0; pid > 1 && hops < 64; hops++ {
		ns, err := os.Readlink(fmt.Sprintf("/proc/%d/ns/mnt", pid))
		if err != nil {
			return nil // refused: the walk is closed
		}
		if ns != own {
			return fmt.Errorf("process %d is outside the walls and can be read through /proc, so its folders can be written through /proc/%d/root", pid, pid)
		}
		if pid = parentOf(pid); pid < 0 {
			return nil
		}
	}
	return nil
}

// parentOf is the parent of a process, read from its stat line (the field
// after the command's closing bracket and the state), -1 when it cannot be
// read.
func parentOf(pid int) int {
	b, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return -1
	}
	fields := strings.Fields(string(b[bytes.LastIndexByte(b, ')')+1:]))
	if len(fields) < 2 {
		return -1
	}
	parent, err := strconv.Atoi(fields[1])
	if err != nil {
		return -1
	}
	return parent
}

// Refusal is the message of a unit that would not run, as its log and the
// daemon's note carry it: what the check found, and what to do.
func Refusal(cause error) string {
	text := cause.Error()
	if !strings.HasSuffix(text, ".") {
		text += "."
	}
	return text + " " + Fix
}

// Fix is what a person does about walls that are not up.
const Fix = "Open Settings > Sandbox to see what this host lacks and the command that fixes it, or switch the sandbox off there."
