package listeners

import (
	"os"
	"path/filepath"
	"strconv"
	"testing"
)

// fakeProc builds a /proc lookalike: net/tcp{,6} tables plus one directory
// per process with environ, comm and fd symlinks — the only files Scan reads.
type fakeProc struct {
	t    *testing.T
	root string
	tcp  string
	tcp6 string
}

func newFakeProc(t *testing.T) *fakeProc {
	t.Helper()
	f := &fakeProc{t: t, root: t.TempDir()}
	f.tcp = "  sl  local_address rem_address   st tx_queue rx_queue tr tm->when retrnsmt   uid  timeout inode\n"
	f.tcp6 = f.tcp
	return f
}

// listen adds a LISTEN row: hex address:port, state 0A, the given inode.
func (f *fakeProc) listen(v6 bool, addrHex string, port, inode int) {
	line := "   0: " + addrHex + ":" + strconv.FormatInt(int64(port), 16) + " 00000000:0000 0A 00000000:00000000 00:00000000 00000000  1002        0 " +
		strconv.Itoa(inode) + " 1 0000000000000000 100 0 0 10 0\n"
	if v6 {
		f.tcp6 += line
	} else {
		f.tcp += line
	}
}

// established adds a non-LISTEN row (state 01) that must be ignored.
func (f *fakeProc) established(port, inode int) {
	f.tcp += "   1: 0100007F:" + strconv.FormatInt(int64(port), 16) + " 0100007F:1F90 01 00000000:00000000 00:00000000 00000000  1002        0 " +
		strconv.Itoa(inode) + " 1 0000000000000000 100 0 0 10 0\n"
}

// process adds /proc/<pid> with the given comm, env (KEY=VALUE lines) and
// socket inodes as fd symlinks.
func (f *fakeProc) process(pid int, comm string, env []string, sockets ...int) {
	dir := filepath.Join(f.root, strconv.Itoa(pid))
	if err := os.MkdirAll(filepath.Join(dir, "fd"), 0o755); err != nil {
		f.t.Fatal(err)
	}
	environ := ""
	for _, kv := range env {
		environ += kv + "\x00"
	}
	must(f.t, os.WriteFile(filepath.Join(dir, "environ"), []byte(environ), 0o644))
	must(f.t, os.WriteFile(filepath.Join(dir, "comm"), []byte(comm+"\n"), 0o644))
	must(f.t, os.WriteFile(filepath.Join(dir, "stat"), []byte(strconv.Itoa(pid)+" ("+comm+") S 1 1 1 0 -1 4194560 0 0 0 0 0 0 0 0 20 0 1 0 12345 0 0\n"), 0o644))
	for i, ino := range sockets {
		must(f.t, os.Symlink("socket:["+strconv.Itoa(ino)+"]", filepath.Join(dir, "fd", strconv.Itoa(3+i))))
	}
	// A non-socket fd, as every real process has.
	must(f.t, os.Symlink("/dev/null", filepath.Join(dir, "fd", "0")))
}

func (f *fakeProc) scanner() *Scanner {
	must(f.t, os.MkdirAll(filepath.Join(f.root, "net"), 0o755))
	must(f.t, os.WriteFile(filepath.Join(f.root, "net", "tcp"), []byte(f.tcp), 0o644))
	must(f.t, os.WriteFile(filepath.Join(f.root, "net", "tcp6"), []byte(f.tcp6), 0o644))
	return NewAt(f.root)
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func TestScan_AttributesListenersToPanes(t *testing.T) {
	f := newFakeProc(t)
	f.listen(false, "00000000", 3003, 100) // next on 0.0.0.0:3003
	f.listen(true, "00000000000000000000000000000000", 8082, 101)
	f.listen(false, "0100007F", 5432, 102) // postgres, not a pane process
	f.established(3003, 103)               // an accepted connection on 3003, not a listener
	f.process(10, "next-server", []string{"HOME=/h", "CCMUX_PANE_ID=pane-a"}, 100)
	f.process(11, "node", []string{"CCMUX_PANE_ID=pane-b"}, 101, 103)
	f.process(12, "postgres", []string{"HOME=/h"}, 102)
	f.process(13, "zsh", []string{"CCMUX_PANE_ID=pane-a"}) // a pane shell with no sockets

	got, err := f.scanner().Scan()
	if err != nil {
		t.Fatal(err)
	}
	want := []Listener{
		{Port: 3003, PID: 10, Process: "next-server", PaneID: "pane-a"},
		{Port: 8082, PID: 11, Process: "node", PaneID: "pane-b"},
	}
	if len(got) != len(want) {
		t.Fatalf("got %+v, want %+v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("row %d: got %+v, want %+v", i, got[i], want[i])
		}
	}
}

func TestScan_OnePortListedOnce(t *testing.T) {
	// A server bound on both v4 and v6 (or a parent and its worker sharing the
	// socket) must not produce two rows for one port.
	f := newFakeProc(t)
	f.listen(false, "00000000", 8000, 200)
	f.listen(true, "00000000000000000000000000000000", 8000, 201)
	f.process(20, "uvicorn", []string{"CCMUX_PANE_ID=p"}, 200, 201)
	f.process(21, "uvicorn", []string{"CCMUX_PANE_ID=p"}, 200, 201) // forked worker
	got, err := f.scanner().Scan()
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Port != 8000 || got[0].PaneID != "p" {
		t.Fatalf("got %+v, want one row for 8000", got)
	}
}

func TestScan_NoProcIsAnError(t *testing.T) {
	if _, err := NewAt(filepath.Join(t.TempDir(), "missing")).Scan(); err == nil {
		t.Fatal("expected an error without a proc tree (macOS, containers without /proc)")
	}
}

func TestScan_CachesPaneByPID(t *testing.T) {
	// The pane id of a process never changes while it lives, so environ is
	// read once per (pid, start time). A recycled pid with a new start time
	// is re-read.
	f := newFakeProc(t)
	f.listen(false, "00000000", 4000, 300)
	f.process(30, "node", []string{"CCMUX_PANE_ID=first"}, 300)
	s := f.scanner()
	if got, _ := s.Scan(); len(got) != 1 || got[0].PaneID != "first" {
		t.Fatalf("first scan: %+v", got)
	}
	// Same pid, same start time, environ rewritten: the cache answers.
	must(t, os.WriteFile(filepath.Join(f.root, "30", "environ"), []byte("CCMUX_PANE_ID=changed\x00"), 0o644))
	if got, _ := s.Scan(); len(got) != 1 || got[0].PaneID != "first" {
		t.Fatalf("cached scan: %+v", got)
	}
	// New start time = a recycled pid: re-read.
	must(t, os.WriteFile(filepath.Join(f.root, "30", "stat"), []byte("30 (node) S 1 1 1 0 -1 4194560 0 0 0 0 0 0 0 0 20 0 1 0 99999 0 0\n"), 0o644))
	if got, _ := s.Scan(); len(got) != 1 || got[0].PaneID != "changed" {
		t.Fatalf("recycled-pid scan: %+v", got)
	}
}
