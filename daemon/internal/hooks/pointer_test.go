package hooks

import (
	"bufio"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// sockDir is a SHORT temp dir. t.TempDir() on macOS returns a
// /var/folders/…/TestNameNNNN/001 path that blows past the ~104-byte sun_path
// limit, and bind fails with "invalid argument" — which, skipped, silently
// turned all of these into no-ops.
func sockDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "ccmuxhook")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	return dir
}

// listenOnce binds a unix socket and returns a channel carrying the first line
// delivered to it — the proof a hook actually arrived somewhere, rather than
// being written to a path nothing is listening on.
func listenOnce(t *testing.T, path string) <-chan string {
	t.Helper()
	ln, err := net.Listen("unix", path)
	if err != nil {
		t.Fatalf("bind %s: %v", path, err)
	}
	t.Cleanup(func() { ln.Close() })
	got := make(chan string, 1)
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		line, _ := bufio.NewReader(conn).ReadString('\n')
		got <- line
	}()
	return got
}

// TestNotifyScript_StaleSocketFallsBackToPointer is the regression that cost
// weeks: a pane created before the daemon's socket moved kept sending to the old
// path, every hook was dropped in silence, and the peers bus concluded those
// sessions were not there. The frozen value must not be trusted when the path
// it names is gone.
func TestNotifyScript_StaleSocketFallsBackToPointer(t *testing.T) {
	requireScript(t)
	dir := sockDir(t)
	live := filepath.Join(dir, "hooks.sock")
	got := listenOnce(t, live)

	pointer := filepath.Join(dir, "hooks-socket")
	if err := WritePointer(pointer, live); err != nil {
		t.Fatal(err)
	}

	cmd := exec.Command("bash", notifyScript, "session-start")
	cmd.Stdin = strings.NewReader(`{"session_id":"s1","cwd":"/repo","hook_event_name":"SessionStart"}`)
	cmd.Env = append(scrubbedEnv(),
		"CCMUX_HOOK_TRACE="+filepath.Join(dir, "trace.jsonl"),
		"CCMUX_PANE_ID=pane-1",
		"CCMUX_HOOKS_SOCK="+filepath.Join(dir, "gone.sock"), // the frozen, dead path
		"CCMUX_HOOKS_POINTER="+pointer,
	)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("script failed: %v\n%s", err, out)
	}

	select {
	case line := <-got:
		if !strings.Contains(line, "session_start") {
			t.Errorf("delivered payload = %q, want the session_start event", line)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("nothing reached the live socket — a stale CCMUX_HOOKS_SOCK still swallows every hook")
	}
}

// The pointer is a fallback, never an override: a pane whose socket is alive
// must keep using it. Otherwise every hosted pane on a host would pile onto
// whichever daemon wrote the pointer last.
func TestNotifyScript_LiveSocketIgnoresPointer(t *testing.T) {
	requireScript(t)
	dir := sockDir(t)
	own := filepath.Join(dir, "own.sock")
	other := filepath.Join(dir, "other.sock")
	ownGot := listenOnce(t, own)
	otherGot := listenOnce(t, other)

	pointer := filepath.Join(dir, "hooks-socket")
	if err := WritePointer(pointer, other); err != nil {
		t.Fatal(err)
	}

	cmd := exec.Command("bash", notifyScript, "session-start")
	cmd.Stdin = strings.NewReader(`{"session_id":"s1","cwd":"/repo","hook_event_name":"SessionStart"}`)
	cmd.Env = append(scrubbedEnv(),
		"CCMUX_HOOK_TRACE="+filepath.Join(dir, "trace.jsonl"),
		"CCMUX_PANE_ID=pane-1",
		"CCMUX_HOOKS_SOCK="+own,
		"CCMUX_HOOKS_POINTER="+pointer,
	)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("script failed: %v\n%s", err, out)
	}

	select {
	case <-ownGot:
	case <-otherGot:
		t.Fatal("the pointer overrode a live socket; it must only cover a dead one")
	case <-time.After(5 * time.Second):
		t.Fatal("nothing arrived at either socket")
	}
}

// A local pane driven by the Mac app carries CCMUX_CMD_FILE and no
// CCMUX_HOOKS_SOCK, and belongs to the app's socket. The daemon's pointer must
// not capture it — that collision is exactly what the two separate paths exist
// to prevent.
func TestNotifyScript_AppPaneIgnoresDaemonPointer(t *testing.T) {
	requireScript(t)
	dir := sockDir(t)
	daemonSock := filepath.Join(dir, "daemon.sock")
	daemonGot := listenOnce(t, daemonSock)

	pointer := filepath.Join(dir, "hooks-socket")
	if err := WritePointer(pointer, daemonSock); err != nil {
		t.Fatal(err)
	}
	trace := filepath.Join(dir, "trace.jsonl")

	cmd := exec.Command("bash", notifyScript, "session-start")
	cmd.Stdin = strings.NewReader(`{"session_id":"s1","cwd":"/repo","hook_event_name":"SessionStart"}`)
	cmd.Env = append(scrubbedEnv(),
		"CCMUX_HOOK_TRACE="+trace,
		"CCMUX_CMD_FILE="+filepath.Join(dir, "ccmux-cmd"), // app pane: no CCMUX_HOOKS_SOCK
		"CCMUX_HOOKS_POINTER="+pointer,
	)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("script failed: %v\n%s", err, out)
	}

	select {
	case <-daemonGot:
		t.Fatal("an app pane's hook was captured by the daemon's pointer")
	case <-time.After(500 * time.Millisecond):
	}
	written, err := os.ReadFile(trace)
	if err != nil {
		t.Fatalf("app pane wrote no trace at all: %v", err)
	}
	if !strings.Contains(string(written), "/tmp/ccmux-hooks.sock") {
		t.Errorf("app pane did not address the app socket:\n%s", written)
	}
}

func TestWritePointer(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "hooks-socket")
	if err := WritePointer(path, "/run/ccmuxd/hooks.sock"); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	// Trailing newline: the script reads it with `read -r`, which needs one to
	// return the line on some shells, and it keeps the file catable.
	if string(b) != "/run/ccmuxd/hooks.sock\n" {
		t.Errorf("contents = %q", b)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o644 {
		t.Errorf("mode = %v, want 0644 — a pane's hook must be able to read it", info.Mode().Perm())
	}
}
