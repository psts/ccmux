package walls

import (
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

// walled is a system in which the walls are up: another mount namespace,
// every write refused as read-only but in the folders named.
func walled(open ...string) probes {
	return probes{
		mountNS: func() (string, error) { return "mnt:[2]", nil },
		tryWrite: func(path string) error {
			for _, o := range open {
				if path == o {
					return nil
				}
			}
			return &os.PathError{Op: "open", Path: path, Err: syscall.EROFS}
		},
		canWrite: func(string) error { return nil },
		connect:  func(path string) error { return &os.PathError{Op: "connect", Path: path, Err: syscall.EACCES} },
		interfaces: func() ([]net.Interface, error) {
			return []net.Interface{{Name: "lo", Flags: net.FlagLoopback | net.FlagUp}}, nil
		},
	}
}

var expect = Expect{HostNS: "mnt:[1]", Writable: []string{"/wt"}, Refused: []string{"/home/u", "/repo/.git/hooks"}}

func TestAUnitBehindItsWallsPasses(t *testing.T) {
	if err := walled().check(expect); err != nil {
		t.Fatal(err)
	}
}

func TestAUnitThatIsNotWalledRefusesToRun(t *testing.T) {
	cases := map[string]struct {
		sys  func(p *probes)
		e    Expect
		want string
	}{
		"the daemon's own namespace": {
			sys:  func(p *probes) { p.mountNS = func() (string, error) { return "mnt:[1]", nil } },
			e:    expect,
			want: "systemd built no walls",
		},
		"a write outside is taken": {
			sys:  func(p *probes) { *p = walled("/home/u") },
			e:    expect,
			want: "/home/u took a write",
		},
		"the hooks are open": {
			sys:  func(p *probes) { *p = walled("/repo/.git/hooks") },
			e:    expect,
			want: "/repo/.git/hooks took a write",
		},
		"refused, but by the file's own mode": {
			sys: func(p *probes) {
				p.tryWrite = func(path string) error { return &os.PathError{Op: "open", Path: path, Err: syscall.EACCES} }
			},
			e:    expect,
			want: "proves no wall",
		},
		"its own folder is read-only": {
			sys:  func(p *probes) { p.canWrite = func(string) error { return syscall.EROFS } },
			e:    expect,
			want: "/wt, which its work writes, cannot be written",
		},
		"a spec that names no namespace": {
			sys:  func(*probes) {},
			e:    Expect{Refused: []string{"/home/u"}},
			want: "does not name the daemon's mount namespace",
		},
		"a spec that proves nothing": {
			sys:  func(*probes) {},
			e:    Expect{HostNS: "mnt:[1]"},
			want: "nothing proves a wall",
		},
		"the user's bus takes a connection": {
			// It is on a read-only mount, so it reads as unwritable: only a
			// real connection tells.
			sys: func(p *probes) {
				p.canWrite = func(string) error { return syscall.EROFS }
				p.connect = func(string) error { return nil }
			},
			e:    Expect{HostNS: "mnt:[1]", Refused: []string{"/home/u"}, Hidden: []string{"/run/user/1000/bus"}},
			want: "/run/user/1000/bus takes a connection",
		},
		"a network where none was to be": {
			sys: func(p *probes) {
				p.interfaces = func() ([]net.Interface, error) {
					return []net.Interface{{Name: "lo", Flags: net.FlagLoopback}, {Name: "eth0", Flags: net.FlagUp}}, nil
				}
			},
			e:    Expect{HostNS: "mnt:[1]", Refused: []string{"/home/u"}, NoNetwork: true},
			want: "eth0 is there",
		},
	}
	for name, c := range cases {
		p := walled()
		c.sys(&p)
		err := p.check(c.e)
		if !errors.Is(err, ErrNoWalls) {
			t.Errorf("%s: want a refusal, got %v", name, err)
			continue
		}
		if !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: the refusal does not say why (%q): %v", name, c.want, err)
		}
	}
}

func TestAHiddenSocketThatTakesNoConnectionPasses(t *testing.T) {
	e := Expect{HostNS: "mnt:[1]", Writable: []string{"/wt"}, Refused: []string{"/home/u"}, Hidden: []string{"/run/user/1000/bus", "/run/user/1000/not-there"}}
	if err := walled().check(e); err != nil {
		t.Fatal(err)
	}
}

// The real probe against a real socket: one that listens takes the
// connection, one that is gone does not.
func TestTheRealConnectProbe(t *testing.T) {
	path := filepath.Join(t.TempDir(), "bus")
	l, err := net.Listen("unix", path)
	if err != nil {
		t.Skipf("no unix sockets here: %v", err)
	}
	if err := connect(path); err != nil {
		t.Fatalf("a listening socket takes a connection: %v", err)
	}
	l.Close()
	if err := connect(path); err == nil {
		t.Fatal("a socket that is gone takes none")
	}
}

func TestAUnitWithNoNetworkAndOnlyLoopbackPasses(t *testing.T) {
	e := Expect{HostNS: "mnt:[1]", Refused: []string{"/home/u"}, NoNetwork: true}
	if err := walled().check(e); err != nil {
		t.Fatal(err)
	}
}

// The real probes, on this test's own open folders: a write that works is
// undone, and the check then says the folder is not walled.
func TestTheRealProbesSeeAnOpenFolderAndLeaveNothingBehind(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "config")
	if err := os.WriteFile(file, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := tryWrite(dir); err != nil {
		t.Fatalf("an open folder takes the write: %v", err)
	}
	if err := tryWrite(file); err != nil {
		t.Fatalf("an open file takes the write: %v", err)
	}
	left, _ := os.ReadDir(dir)
	if b, _ := os.ReadFile(file); len(left) != 1 || string(b) != "x" {
		t.Fatalf("the probe left a trace: %v, config=%q", left, b)
	}
	if err := canWrite(dir); err != nil {
		t.Fatalf("canWrite: %v", err)
	}
	if err := tryWrite(filepath.Join(dir, "missing")); err == nil {
		t.Fatal("a path that is not there cannot take a write")
	}
	// This test's process is not walled: the whole check must refuse.
	ns, err := MountNS()
	if err != nil {
		t.Skipf("no /proc here: %v", err)
	}
	if err := Check(Expect{HostNS: ns, Refused: []string{dir}}); !errors.Is(err, ErrNoWalls) {
		t.Fatalf("an unwalled process must not pass: %v", err)
	}
	if err := Check(Expect{HostNS: "mnt:[0]", Refused: []string{dir}}); !errors.Is(err, ErrNoWalls) || !strings.Contains(err.Error(), "took a write") {
		t.Fatalf("another namespace alone is not a wall: %v", err)
	}
}

func TestARefusalSaysWhatToDo(t *testing.T) {
	text := Refusal(noWalls("it runs in the daemon's own mount namespace"))
	if !strings.Contains(text, "the sandbox is on") || !strings.Contains(text, "Settings > Sandbox") {
		t.Fatalf("refusal: %s", text)
	}
}
