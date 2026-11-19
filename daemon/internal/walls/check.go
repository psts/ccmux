package walls

import (
	"errors"
	"fmt"
	"net"
	"os"
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
	connect    func(path string) error
	interfaces func() ([]net.Interface, error)
}

var system = probes{mountNS: MountNS, tryWrite: tryWrite, canWrite: canWrite, connect: connect, interfaces: net.Interfaces}

// Check proves the walls are up around this process, and says why not
// otherwise (an ErrNoWalls). A unit calls it first thing and does no work
// on an error: a unit that is meant to be walled never runs open.
//
// These must hold. The process is in a mount namespace of its own (not
// the daemon's). Every path it must not write refuses a real write as a
// read-only file system: "permission denied" would not do, since that is
// the file's own mode and proves no wall. Every folder it is meant to
// write takes one, or its work would fail later with an error that names
// no wall. And the sockets hidden from it cannot be connected to.
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
	for _, path := range e.Hidden {
		// A real connection, not a question about rights: a socket on a
		// read-only mount reads as unwritable and still takes one. A socket
		// that is not there at all is as good as hidden.
		if err := p.connect(path); err == nil {
			return noWalls(path + " takes a connection, and through it a command runs outside the walls")
		}
	}
	if e.NoNetwork {
		return p.mustBeOffline()
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

// connect opens a connection to the socket at path and closes it at once,
// saying nothing on it.
func connect(path string) error {
	c, err := net.DialTimeout("unix", path, 2*time.Second)
	if err != nil {
		return err
	}
	return c.Close()
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
