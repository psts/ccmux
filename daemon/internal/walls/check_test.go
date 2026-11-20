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
		reach:    func(path string) error { return &os.PathError{Op: "open", Path: path, Err: syscall.EACCES} },
		list:     func(string) ([]string, error) { return nil, nil },
		outside:  func() error { return nil },
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
				p.reach = func(string) error { return nil }
			},
			e:    Expect{HostNS: "mnt:[1]", Refused: []string{"/home/u"}, Hidden: []string{"/run/user/1000/bus"}},
			want: "/run/user/1000/bus can be reached",
		},
		"another session's spec can be seen": {
			sys: func(p *probes) {
				p.list = func(string) ([]string, error) { return []string{"s1.json", "s1.own", "s2.json"}, nil }
			},
			e:    Expect{HostNS: "mnt:[1]", Refused: []string{"/home/u"}, Only: map[string][]string{"/state": {"s1.json", "s1.own"}}},
			want: "/state shows s2.json, which is another unit's",
		},
		"a shared folder that cannot be listed": {
			sys: func(p *probes) {
				p.list = func(string) ([]string, error) { return nil, syscall.EACCES }
			},
			e:    Expect{HostNS: "mnt:[1]", Refused: []string{"/home/u"}, Only: map[string][]string{"/state": {"s1.json"}}},
			want: "/state, of which it is to see its own part only, cannot be listed",
		},
		"a process outside can be walked into": {
			sys: func(p *probes) {
				p.outside = func() error { return errors.New("process 1301 is outside the walls and can be read through /proc") }
			},
			e:    expect,
			want: "process 1301 is outside the walls",
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
func TestTheRealReachProbeOnASocket(t *testing.T) {
	path := filepath.Join(t.TempDir(), "bus")
	l, err := net.Listen("unix", path)
	if err != nil {
		t.Skipf("no unix sockets here: %v", err)
	}
	if err := reach(path); err != nil {
		t.Fatalf("a listening socket takes a connection: %v", err)
	}
	l.Close()
	if err := reach(path); err == nil {
		t.Fatal("a socket that is gone takes none")
	}
}

// The real probe against files and folders: what can be read is reached,
// what the file's mode closes (as an inaccessible path's does) is not.
func TestTheRealReachProbeOnFilesAndFolders(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("root reads everything")
	}
	dir := t.TempDir()
	file, folder := filepath.Join(dir, "api-token"), filepath.Join(dir, "sessions")
	if err := os.WriteFile(file, []byte("secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(folder, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{file, folder} {
		if err := reach(path); err != nil {
			t.Fatalf("%s is open and must read as reached: %v", path, err)
		}
		if err := os.Chmod(path, 0); err != nil {
			t.Fatal(err)
		}
		if err := reach(path); err == nil {
			t.Fatalf("%s is closed and must not read as reached", path)
		}
		os.Chmod(path, 0o700)
	}
	if err := reach(filepath.Join(dir, "not-there")); err == nil {
		t.Fatal("a path that is not there is not reached")
	}
	names, err := list(dir)
	if err != nil || strings.Join(names, " ") != "api-token sessions" {
		t.Fatalf("list: %v %v", names, err)
	}
}

// This test's own process is behind no walls: its parent is in its own
// mount namespace, so the walk up finds nobody outside to walk into.
func TestTheRealOutsideProbeFindsNothingWhereThereAreNoWalls(t *testing.T) {
	if _, err := MountNS(); err != nil {
		t.Skipf("no /proc here: %v", err)
	}
	if err := outside(); err != nil {
		t.Fatalf("a process whose ancestors share its namespace: %v", err)
	}
	if got := parentOf(os.Getpid()); got != os.Getppid() {
		t.Fatalf("parentOf reads the stat line: %d, want %d", got, os.Getppid())
	}
	if got := parentOf(-5); got != -1 {
		t.Fatalf("a process that is not there has no parent: %d", got)
	}
}

func TestAUnitThatSeesOnlyItsOwnPartOfASharedFolderPasses(t *testing.T) {
	p := walled()
	p.list = func(string) ([]string, error) { return []string{"s1.own"}, nil }
	e := Expect{HostNS: "mnt:[1]", Refused: []string{"/home/u"}, Only: map[string][]string{"/state": {"s1.json", "s1.own", "s1.brief.md"}}}
	if err := p.check(e); err != nil {
		t.Fatalf("what it may see and is not there (it has no brief) is no fault: %v", err)
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
