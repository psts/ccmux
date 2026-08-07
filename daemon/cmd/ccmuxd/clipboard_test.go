package main

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"ccmux.dev/ccmuxd/config"
)

// TestRenderTmuxConf pins the substitution contract: the conf the tmux server
// actually loads must carry the helper path everywhere and no placeholder —
// a leftover __CCMUX_COPY__ would make every copy binding run a nonsense
// command, and the pipe's own error handling would hide it forever.
func TestRenderTmuxConf(t *testing.T) {
	got := renderTmuxConf(config.TmuxConf, "/run/ccmuxd/ccmux-copy")
	if strings.Contains(got, "__CCMUX_COPY__") {
		t.Error("placeholder survived substitution")
	}
	// Counted exactly, and with the trailing quote: the hook lines below start
	// with the same prefix, so a `>= 13` on the prefix would still pass after
	// two copy bindings were deleted.
	if n := strings.Count(got, `/run/ccmuxd/ccmux-copy '#{pane_id}'"`); n != 13 {
		t.Errorf("helper wired into %d bindings, want exactly 13 (drag+keys+clicks across tables)", n)
	}
	// The hooks are gone on purpose and must not come back: a tmux buffer hook
	// names the pane that copied but never the buffer it fired for, so under
	// concurrent copies it broadcasts one workspace's text to another's lens.
	for _, line := range strings.Split(got, "\n") {
		if strings.HasPrefix(line, "set-hook") && strings.Contains(line, "buffer") {
			t.Errorf("a buffer hook is back — it cannot bind a copy to its buffer: %s", line)
		}
	}
}

// TestClipboardScript pins the helper's load-bearing parts: token read from
// its 0600 file at request time (not baked into the world-readable-ish conf),
// and stderr to the log — the one failure the daemon can never record itself is
// "the request never arrived".
func TestClipboardScript(t *testing.T) {
	s := clipboardScript("/rt/clipboard-token", "http://127.0.0.1:7900", "/rt/clipboard.log")
	for _, want := range []string{
		`$(cat "/rt/clipboard-token"`,
		`"http://127.0.0.1:7900/v1/clipboard"`,
		`2>>"/rt/clipboard.log"`,
		"X-Ccmux-Pane: $pane",
		// The shim calls with no argument and relies on this: $TMUX_PANE is set
		// by tmux in the copying process, the one id no tmux hook can give us.
		`pane="$TMUX_PANE"`,
	} {
		if !strings.Contains(s, want) {
			t.Errorf("script missing %q:\n%s", want, s)
		}
	}
	// `|| true` blanket-swallowed the daemon's answer for BOTH callers; the exit
	// policy is now split by caller and pinned in TestClipboardScript_ExitPolicy.
	if strings.Contains(s, "|| true") {
		t.Errorf("blanket `|| true` is back — the shim would report copies that never landed:\n%s", s)
	}
}

// writeStub drops an executable /bin/sh script on the fake PATH.
func writeStub(t *testing.T, bin, name, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(bin, name), []byte("#!/bin/sh\n"+body), 0o700); err != nil {
		t.Fatal(err)
	}
}

// clipBed renders the real helper and shim into a temp dir behind a fake curl.
// curlRC is the exit status the fake curl reports, so the tests can drive the
// daemon-said-no path as well as the happy one.
type clipBed struct {
	dir, helper, shim string
	binDir            string
}

func newClipBed(t *testing.T, curlRC int) clipBed {
	t.Helper()
	b := clipBed{dir: t.TempDir()}
	b.binDir = filepath.Join(b.dir, "bin")
	if err := os.MkdirAll(b.binDir, 0o700); err != nil {
		t.Fatal(err)
	}
	writeStub(t, b.binDir, "curl", "printf '%s\\n' \"$*\" >> "+filepath.Join(b.dir, "curl.log")+
		"\ncat >> "+filepath.Join(b.dir, "body.log")+
		fmt.Sprintf("\nexit %d\n", curlRC))
	b.helper = filepath.Join(b.dir, "ccmux-copy")
	body := clipboardScript(filepath.Join(b.dir, "token"), "http://127.0.0.1:7900", filepath.Join(b.dir, "log"))
	if err := os.WriteFile(b.helper, []byte(body), 0o700); err != nil {
		t.Fatal(err)
	}
	b.shim = filepath.Join(b.dir, "xclip")
	if err := os.WriteFile(b.shim, []byte(clipboardShimScript(b.helper)), 0o700); err != nil {
		t.Fatal(err)
	}
	return b
}

// run executes script with args and $TMUX_PANE=pane, returning its exit code.
func (b clipBed) run(t *testing.T, script string, args []string, pane string) int {
	t.Helper()
	cmd := exec.Command(script, args...)
	cmd.Env = append(os.Environ(), "PATH="+b.binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	if pane != "" {
		cmd.Env = append(cmd.Env, "TMUX_PANE="+pane)
	}
	cmd.Stdin = strings.NewReader("café copied")
	if err := cmd.Run(); err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			return ee.ExitCode()
		}
		t.Fatalf("%s: %v", script, err)
	}
	return 0
}

func (b clipBed) read(name string) string {
	out, _ := os.ReadFile(filepath.Join(b.dir, name))
	return string(out)
}

// TestClipboardScript_Runs executes the rendered helper for real: a shell syntax
// error would otherwise reach the tmux server, where the log-to-file redirect
// would hide it forever.
func TestClipboardScript_Runs(t *testing.T) {
	b := newClipBed(t, 0)

	if rc := b.run(t, b.helper, []string{"%7"}, ""); rc != 0 { // binding: pane as argv
		t.Errorf("binding call exited %d, want 0", rc)
	}
	if rc := b.run(t, b.helper, nil, "%9"); rc != 0 { // shim: pane from the environment
		t.Errorf("shim call exited %d, want 0", rc)
	}

	log := b.read("curl.log")
	if !strings.Contains(log, "X-Ccmux-Pane: %7") || !strings.Contains(log, "X-Ccmux-Pane: %9") {
		t.Errorf("argv and $TMUX_PANE panes did not both reach curl:\n%s", log)
	}
	if got := b.read("body.log"); got != "café copiedcafé copied" {
		t.Errorf("bodies = %q, want both copies byte-for-byte", got)
	}
}

// TestClipboardScript_ExitPolicy pins the split that keeps copy-mode unbreakable
// while still telling an app the truth. A tmux binding must never fail — the
// daemon being down cannot break copy-mode — but the shim's exit status is the
// copying app's only signal, so a 413 or a dead daemon has to travel back.
func TestClipboardScript_ExitPolicy(t *testing.T) {
	cases := []struct {
		name   string
		args   []string
		pane   string
		curlRC int
		want   int
	}{
		{"binding, daemon refuses", []string{"%1"}, "", 22, 0},
		{"binding with a blank pane is still a binding", []string{""}, "", 0, 0},
		{"shim, daemon refuses", nil, "%1", 22, 1},
		{"shim, no pane anywhere", nil, "", 0, 1},
		{"shim, daemon accepts", nil, "%1", 0, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			b := newClipBed(t, tc.curlRC)
			if rc := b.run(t, b.helper, tc.args, tc.pane); rc != tc.want {
				t.Errorf("exit = %d, want %d (log: %s)", rc, tc.want, b.read("log"))
			}
		})
	}
}

// TestClipboardShimScript_Runs pins the argv contract. The shim sits at the
// front of every hosted pane's PATH, so it shadows a real xclip for every
// command in the pane — the one thing it must never do is report a copy it did
// not perform.
func TestClipboardShimScript_Runs(t *testing.T) {
	b := newClipBed(t, 0)

	if rc := b.run(t, b.shim, []string{"-selection", "clipboard"}, "%3"); rc != 0 {
		t.Errorf("write exited %d, want 0", rc)
	}
	if got := b.read("curl.log"); !strings.Contains(got, "X-Ccmux-Pane: %3") {
		t.Errorf("write did not reach the helper:\n%s", got)
	}

	// Everything below must post NOTHING.
	refusals := []struct {
		name string
		args []string
		want int
	}{
		{"read is not available on this host", []string{"-o"}, 1},
		{"read, xclip image form", []string{"-selection", "clipboard", "-t", "image/png", "-o"}, 1},
		{"PRIMARY is declined, long flag", []string{"--primary"}, 0},
		{"PRIMARY is declined, short flag", []string{"-p"}, 0},
		{"PRIMARY is declined as a selection value", []string{"-selection", "primary"}, 0},
		{"text as an argument is refused, not silently dropped", []string{"hello world"}, 1},
		{"a file argument is refused too", []string{"notes.txt"}, 1},
	}
	for _, r := range refusals {
		t.Run(r.name, func(t *testing.T) {
			if rc := b.run(t, b.shim, r.args, "%3"); rc != r.want {
				t.Errorf("exit = %d, want %d", rc, r.want)
			}
		})
	}
	if n := strings.Count(b.read("curl.log"), "X-Ccmux-Pane"); n != 1 {
		t.Errorf("only the one real write should post; got %d:\n%s", n, b.read("curl.log"))
	}
}

// TestWriteClipboardShims pins the platform gate: only a headless Linux host
// needs the fake tools, and every name Claude Code probes for must be present
// (it looks for wl-copy first, then xclip, then xsel).
func TestWriteClipboardShims(t *testing.T) {
	dir := t.TempDir()
	shimDir, err := writeClipboardShims(dir, "/rt/ccmux-copy")
	if err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "linux" {
		if shimDir != "" {
			t.Errorf("shim dir = %q on %s, want none — the platform clipboard already writes the lens's machine", shimDir, runtime.GOOS)
		}
		return
	}
	for _, name := range []string{"xclip", "wl-copy", "xsel"} {
		fi, err := os.Stat(filepath.Join(shimDir, name))
		if err != nil {
			t.Errorf("missing %s shim: %v", name, err)
			continue
		}
		if fi.Mode().Perm()&0o100 == 0 {
			t.Errorf("%s shim is not executable (%v)", name, fi.Mode())
		}
	}
}

// TestLoopbackURL pins the address→origin mapping the clipboard pipe and pane
// env both depend on; "" out of this function silently produces a curl to
// nowhere.
func TestLoopbackURL(t *testing.T) {
	cases := map[string]string{
		"127.0.0.1:7900": "http://127.0.0.1:7900",
		"0.0.0.0:7900":   "http://127.0.0.1:7900",
		":7900":          "http://127.0.0.1:7900",
		"[::]:7900":      "http://127.0.0.1:7900",
		"garbage":        "",
	}
	for in, want := range cases {
		if got := loopbackURL(in); got != want {
			t.Errorf("loopbackURL(%q) = %q, want %q", in, got, want)
		}
	}
}
