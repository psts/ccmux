package main

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
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
	// tmux's default update-environment list starts with DISPLAY, so an attaching
	// client with no DISPLAY marks it REMOVED for the whole session — which
	// silently disabled the clipboard shim, since Claude Code only looks for a
	// clipboard tool when a display is claimed. Verified in a Debian container.
	if !strings.Contains(got, `set -g update-environment ""`) {
		t.Error("update-environment is no longer cleared — an attaching client will strip DISPLAY from panes")
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

// TestClipboardShimScript_Dash re-runs the argv contract under dash, which is
// /bin/sh on Debian and Ubuntu — the hosts this shim is written for. macOS
// /bin/sh is bash and forgives things dash does not.
func TestClipboardShimScript_Dash(t *testing.T) {
	dash, err := exec.LookPath("dash")
	if err != nil {
		t.Skip("dash not installed")
	}
	b := newClipBed(t, 0)
	for _, tc := range []struct {
		name string
		args []string
		want int
	}{
		{"write", []string{"-selection", "clipboard"}, 0},
		{"trailing flag with no value", []string{"-selection"}, 1},
		{"read", []string{"-o"}, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cmd := exec.Command(dash, append([]string{b.shim}, tc.args...)...)
			cmd.Env = append(os.Environ(), "PATH="+b.binDir+string(os.PathListSeparator)+os.Getenv("PATH"), "TMUX_PANE=%3")
			cmd.Stdin = strings.NewReader("dash copy")
			rc := 0
			if err := cmd.Run(); err != nil {
				var ee *exec.ExitError
				if !errors.As(err, &ee) {
					t.Fatal(err)
				}
				rc = ee.ExitCode()
			}
			if rc != tc.want {
				t.Errorf("dash exit = %d, want %d", rc, tc.want)
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
		// A value-taking flag in final position. `shift 2 || shift` looked like a
		// guard but dash aborts the script on a shift it cannot do, and dash is
		// /bin/sh on the only platform that gets a shim.
		{"trailing flag with no value", []string{"-selection"}, 1},
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

// TestOwnedByUs is the guard that keeps a real clipboard tool safe: the shim dir
// is the user's own bin directory, so an upgrade must be able to replace its own
// file and must never overwrite someone else's xclip.
func TestOwnedByUs(t *testing.T) {
	dir := t.TempDir()
	absent := filepath.Join(dir, "nothing-here")
	ours := filepath.Join(dir, "ours")
	theirs := filepath.Join(dir, "theirs")
	if err := os.WriteFile(ours, []byte(clipboardShimScript("/rt/ccmux-copy")), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(theirs, []byte("#!/bin/sh\n# the real xclip\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name string
		path string
		want bool
	}{
		{"absent is free to take", absent, true},
		{"our own shim is replaceable", ours, true},
		{"a real tool is left alone", theirs, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := ownedByUs(tc.path); got != tc.want {
				t.Errorf("ownedByUs = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestShimResolves runs a REAL login shell against a temp bin dir. Asserting on
// PATH strings would prove nothing — the reason this check exists is that what
// lands on PATH depends on the shell AND the distro's profile (a Debian bash
// login shell drops an injected PATH; an Ubuntu one keeps it), so the only
// honest answer comes from asking the shell itself.
func TestShimResolves(t *testing.T) {
	sh, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("bash not installed")
	}
	dir := t.TempDir()
	// A REAL shim: shimResolves checks ownership by content now, because a
	// path-only match let a genuine tool sitting in the same directory arm the
	// display claim and swallow every copy.
	shim := []byte(clipboardShimScript("/rt/ccmux-copy"))
	if err := os.WriteFile(filepath.Join(dir, shimNames[0]), shim, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("SHELL", sh)

	// A login shell that puts our dir first must resolve to our shim...
	t.Setenv("BASH_ENV", "")
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	if !shimResolves(dir) {
		t.Error("shim on PATH first was not detected — the display claim would never arm")
	}
	// A real tool at the very same path must NOT arm it.
	if err := os.WriteFile(filepath.Join(dir, shimNames[0]), []byte("#!/bin/sh\n# the real thing\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if shimResolves(dir) {
		t.Error("a foreign tool at the shim's own path armed the display claim")
	}
	// ...and a directory that is NOT on PATH must report honestly, rather than
	// letting the daemon claim a display for a shim nothing can find.
	if shimResolves(filepath.Join(dir, "elsewhere")) {
		t.Error("a dir that is not on PATH reported as resolving")
	}
}

// TestRealClipboardTool covers the guard that keeps this off a machine that
// already has a clipboard. The shim dir is first on PATH, so installing there
// shadows a real xclip for every command the user runs — and a pane doing
// `xclip < ~/.ssh/id_rsa` would post that key to every attached lens.
//
// The search path is passed in because the one that matters is the LOGIN
// SHELL's: the daemon's own PATH under systemd is six system directories, so a
// tool from snap, nix, brew or ~/.local/bin was invisible here while sitting
// ahead of nothing on the user's real PATH.
func TestRealClipboardTool(t *testing.T) {
	binDir := t.TempDir()
	otherDir := t.TempDir()
	sep := string(os.PathListSeparator)
	search := binDir + sep + otherDir

	if got := realClipboardTool(search); got != "" {
		t.Fatalf("a bare host reported %q, want none", got)
	}
	// Our own shim must not count as the host being equipped, even though it
	// shares the searched directories — ownership is by content, not location.
	if err := os.WriteFile(filepath.Join(binDir, "xclip"), []byte(clipboardShimScript("/rt/h")), 0o755); err != nil {
		t.Fatal(err)
	}
	if got := realClipboardTool(search); got != "" {
		t.Errorf("our own shim counted as a real tool: %q", got)
	}
	// A genuine tool anywhere on that path stops the install — including one
	// sharing the bin dir, which the old binDir skip would have hidden.
	real := filepath.Join(otherDir, "xsel")
	if err := os.WriteFile(real, []byte("#!/bin/sh\n# real xsel\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if got := realClipboardTool(search); got != real {
		t.Errorf("realClipboardTool = %q, want %s", got, real)
	}
}

// TestRealClipboardTool_IgnoresNonExecutable: a same-named directory or a
// non-executable file is not a clipboard tool, and must not block the install.
func TestRealClipboardTool_IgnoresNonExecutable(t *testing.T) {
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, "xclip"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "xsel"), []byte("not executable"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := realClipboardTool(dir); got != "" {
		t.Errorf("realClipboardTool = %q, want none", got)
	}
}

// TestHostHasClipboard_FailsClosed: not knowing the pane's PATH must block the
// install. Shadowing a real tool is the worst outcome available here, so an
// unusable login shell has to read as "cannot rule one out".
func TestHostHasClipboard_FailsClosed(t *testing.T) {
	t.Setenv("SHELL", filepath.Join(t.TempDir(), "no-such-shell"))
	got := hostHasClipboard()
	if !strings.Contains(got, "could not read the login shell") {
		t.Errorf("hostHasClipboard = %q, want it to refuse when PATH is unknown", got)
	}
}

// TestDisplayServer is the only guard that stops a shim landing on a desktop
// that has no clipboard tool installed yet, so its edges matter: a Wayland lock
// file is not a display, and an empty runtime dir is not one either.
func TestDisplayServer(t *testing.T) {
	t.Run("wayland socket counts", func(t *testing.T) {
		dir := t.TempDir()
		t.Setenv("XDG_RUNTIME_DIR", dir)
		if err := os.WriteFile(filepath.Join(dir, "wayland-0"), nil, 0o600); err != nil {
			t.Fatal(err)
		}
		if got := displayServer(); !strings.Contains(got, "wayland-0") {
			t.Errorf("displayServer = %q, want the wayland socket", got)
		}
	})
	t.Run("a lock file alone is not a display", func(t *testing.T) {
		dir := t.TempDir()
		t.Setenv("XDG_RUNTIME_DIR", dir)
		if err := os.WriteFile(filepath.Join(dir, "wayland-0.lock"), nil, 0o600); err != nil {
			t.Fatal(err)
		}
		if got := displayServer(); got != "" {
			t.Errorf("displayServer = %q, want none — a lock file is not a socket", got)
		}
	})
	t.Run("empty runtime dir", func(t *testing.T) {
		t.Setenv("XDG_RUNTIME_DIR", t.TempDir())
		if got := displayServer(); got != "" {
			t.Errorf("displayServer = %q, want none", got)
		}
	})
}

// TestRemoveClipboardShims: uninstall takes back only what it installed.
func TestRemoveClipboardShims(t *testing.T) {
	dir := t.TempDir()
	ours := filepath.Join(dir, "xclip")
	theirs := filepath.Join(dir, "wl-copy")
	if err := os.WriteFile(ours, []byte(clipboardShimScript("/rt/ccmux-copy")), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(theirs, []byte("#!/bin/sh\n# a real wl-copy\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	// The real function, not a copy of its rule: removeShimsIn is what both
	// uninstall and the "host gained a clipboard" cleanup call.
	removeShimsIn(dir)
	if _, err := os.Stat(ours); !os.IsNotExist(err) {
		t.Error("our own shim survived uninstall")
	}
	if _, err := os.Stat(theirs); err != nil {
		t.Error("a real tool was deleted by uninstall")
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
