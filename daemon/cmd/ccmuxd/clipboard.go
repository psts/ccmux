package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// setupClipboardPipe writes the artifacts the copy paths need into the (0700,
// per-user) runtime dir: a per-boot random token and the ccmux-copy helper the
// tmux copy bindings pipe into. On Linux it also installs the xclip/wl-copy
// shim beside the daemon binary; shimReady reports whether the user's shell
// actually resolves it, which is what gates claiming a display.
//
// The token turns "anything on this machine can write every lens's clipboard"
// into a per-USER capability: another account can reach the loopback port but
// cannot read this user's runtime dir. It deliberately does NOT try to exclude
// same-user processes — they can drive the tmux socket directly and fake a
// copy anyway, so a same-user boundary here would be theater.
func setupClipboardPipe(dir, baseURL string) (scriptPath, token string, shimReady bool, err error) {
	raw := make([]byte, 16)
	if _, err := rand.Read(raw); err != nil {
		return "", "", false, fmt.Errorf("mint token: %w", err)
	}
	token = hex.EncodeToString(raw)
	tokenPath := filepath.Join(dir, "clipboard-token")
	if err := os.WriteFile(tokenPath, []byte(token), 0o600); err != nil {
		return "", "", false, fmt.Errorf("write token: %w", err)
	}
	scriptPath = filepath.Join(dir, "ccmux-copy")
	script := clipboardScript(tokenPath, baseURL, filepath.Join(dir, "clipboard.log"))
	if err := os.WriteFile(scriptPath, []byte(script), 0o700); err != nil {
		return "", "", false, fmt.Errorf("write script: %w", err)
	}
	// The shim is an enhancement over the tmux-buffer fallback; the pipe above is
	// the feature, so shim trouble is logged inside and never fails this call.
	return scriptPath, token, installClipboardShims(scriptPath), nil
}

// shimNames are the tools Claude Code probes for on Linux, in its own order.
var shimNames = []string{"wl-copy", "xclip", "xsel"}

// shimMarker identifies a file as ours, so an upgrade may replace it and a real
// clipboard tool never gets clobbered.
const shimMarker = "# ccmux-clipboard-shim"

// installClipboardShims puts the fake clipboard tools where the pane's PATH
// already points — beside the ccmuxd binary, which install.sh puts in
// ~/.local/bin — and reports whether the user's own shell then resolves them.
//
// Placement cannot go through a shell rc file. The first cut prepended the dir
// from the zsh ZDOTDIR proxy, which does nothing on a bash host, and handing
// PATH to tmux with `new-session -e` does not survive either: measured in
// containers, a Debian login shell drops an injected PATH via /etc/profile
// while an Ubuntu one keeps it. Installing into a directory the profile already
// puts on PATH is the one mechanism that does not care which shell you run.
//
// It installs ONLY on a host that has no clipboard of its own, and that
// restriction is enforced rather than assumed. The bin dir is first on PATH, so
// a shim shadows a real xclip for every command the user runs — not just ccmux
// panes — and a pane doing `xclip < ~/.ssh/id_rsa` would then post that key to
// every attached lens. So: no install when a genuine tool exists anywhere else
// on PATH, and none when the machine has a display server at all.
func installClipboardShims(helperPath string) bool {
	if runtime.GOOS != "linux" {
		return false // macOS/Windows copy natively to the lens's own machine
	}
	self, err := resolveSelf()
	if err != nil {
		log.Printf("clipboard shim: cannot locate the ccmuxd binary (%v) — app-made copies will land in a tmux buffer", err)
		return false
	}
	binDir := filepath.Dir(self)
	if reason := hostHasClipboard(binDir); reason != "" {
		log.Printf("clipboard shim: not installing — %s. This host's own clipboard tools are left alone; app-made copies in hosted panes will land in a tmux buffer.", reason)
		removeShimsIn(binDir) // an earlier install must not keep shadowing
		return false
	}
	body := []byte(clipboardShimScript(helperPath))
	for _, name := range shimNames {
		path := filepath.Join(binDir, name)
		if !ownedByUs(path) {
			log.Printf("clipboard shim: %s exists and is not ours — leaving it alone", path)
			continue
		}
		if err := os.WriteFile(path, body, 0o755); err != nil {
			log.Printf("clipboard shim: write %s: %v", path, err)
		}
	}
	return shimResolves(binDir)
}

// hostHasClipboard reports why this host must keep its own clipboard, or "" when
// it has none and the shim is safe to install. Two independent signals, because
// either alone is too weak: a desktop may not have xclip installed yet, and a
// headless box may carry a stray tool from some other package.
func hostHasClipboard(binDir string) string {
	if tool := realClipboardTool(binDir); tool != "" {
		return fmt.Sprintf("%s is already installed at %s", filepath.Base(tool), tool)
	}
	if disp := displayServer(); disp != "" {
		return fmt.Sprintf("this machine has a display server (%s)", disp)
	}
	return ""
}

// realClipboardTool finds a clipboard tool that is NOT one of ours, searching
// PATH with binDir removed — our own shim living there would otherwise answer
// for every name and make the host look equipped.
func realClipboardTool(binDir string) string {
	for _, dir := range filepath.SplitList(os.Getenv("PATH")) {
		if dir == "" || dir == binDir {
			continue
		}
		for _, name := range shimNames {
			path := filepath.Join(dir, name)
			fi, err := os.Stat(path)
			if err != nil || fi.IsDir() || fi.Mode().Perm()&0o111 == 0 {
				continue
			}
			if !ownedByUs(path) { // a shim of ours from an older bin dir does not count
				return path
			}
		}
	}
	return ""
}

// displayServer names this machine's display, or "" for a headless host. It
// looks for the sockets rather than at DISPLAY/WAYLAND_DISPLAY, because the
// daemon's OWN environment does not have them: a systemd user service starts at
// boot, before any graphical login, so an env check would call a desktop
// headless and hijack its clipboard.
func displayServer() string {
	if entries, err := os.ReadDir("/tmp/.X11-unix"); err == nil && len(entries) > 0 {
		return "X11 " + entries[0].Name()
	}
	if dir := os.Getenv("XDG_RUNTIME_DIR"); dir != "" {
		if entries, err := os.ReadDir(dir); err == nil {
			for _, e := range entries {
				if strings.HasPrefix(e.Name(), "wayland-") && !strings.HasSuffix(e.Name(), ".lock") {
					return "Wayland " + e.Name()
				}
			}
		}
	}
	return ""
}

// ownedByUs reports whether path is absent or is a shim this daemon wrote.
// A real clipboard tool sitting there must never be overwritten.
func ownedByUs(path string) bool {
	b, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return true
	}
	if err != nil {
		log.Printf("clipboard shim: cannot read %s (%v) — treating it as not ours", path, err)
		return false
	}
	return strings.Contains(string(b), shimMarker)
}

// shimResolves asks the user's OWN login shell what the clipboard tool resolves
// to, rather than assuming a directory is on PATH. That is the shell-agnostic
// check: the answer comes from whatever startup files that shell actually reads,
// so a bash host, a zsh host, and a distro with an opinionated /etc/profile all
// report honestly instead of being guessed at.
//
// It checks the name Claude Code would pick FIRST, and requires the resolved
// file to be ours by content. Matching on path alone was wrong: where a real
// tool already sat in binDir the write was correctly skipped, then the path
// still matched, a display got claimed, and copies went to a tool with no
// display to write — silently.
func shimResolves(binDir string) bool {
	shell := os.Getenv("SHELL")
	if shell == "" {
		shell = "/bin/sh"
	}
	// Bounded: `-lc` sources the user's whole profile chain, and profiles do
	// network work (version managers, auth probes). This runs before the daemon
	// binds its listener, so an unbounded hang here looks like a dead daemon.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	name := shimNames[0]
	out, err := exec.CommandContext(ctx, shell, "-lc", "command -v "+name).Output()
	if ctx.Err() != nil {
		log.Printf("clipboard shim: %s -lc did not answer within 5s — not claiming a display", shell)
		return false
	}
	if err != nil {
		stderr := ""
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			stderr = strings.TrimSpace(string(ee.Stderr))
		}
		log.Printf("clipboard shim: %s could not resolve %s (%v %s) — not claiming a display; app-made copies will land in a tmux buffer", shell, name, err, stderr)
		return false
	}
	got := strings.TrimSpace(string(out))
	if got != filepath.Join(binDir, name) {
		log.Printf("clipboard shim: %s resolves %s to %q, not our shim — not claiming a display; put %s on PATH to enable app-made copies", shell, name, got, binDir)
		return false
	}
	if !ownedByUs(got) {
		log.Printf("clipboard shim: %s is on PATH but is not our shim — not claiming a display", got)
		return false
	}
	return true
}

// removeClipboardShims deletes the shims this daemon installed, leaving any real
// tool that happens to share the name. Called from uninstall.
func removeClipboardShims() {
	self, err := resolveSelf()
	if err != nil {
		log.Printf("uninstall: cannot locate the ccmuxd binary (%v) — clipboard shims may remain on PATH; remove %v from your bin dir by hand", err, shimNames)
		return
	}
	removeShimsIn(filepath.Dir(self))
}

// removeShimsIn deletes our shims from dir, reporting anything it could not take
// back. A survivor shadows a real clipboard tool with a script that posts to a
// port nothing is listening on, so silence here is the one unacceptable outcome.
func removeShimsIn(dir string) {
	for _, name := range shimNames {
		path := filepath.Join(dir, name)
		b, err := os.ReadFile(path)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			log.Printf("clipboard shim: cannot read %s (%v) — leaving it; check it by hand", path, err)
			continue
		}
		if !strings.Contains(string(b), shimMarker) {
			continue
		}
		if err := os.Remove(path); err != nil {
			log.Printf("clipboard shim: could not remove %s (%v) — it will shadow a real clipboard tool; remove it by hand", path, err)
		}
	}
}

// clipboardScript renders the helper the tmux copy bindings invoke: $1 is the
// tmux pane id, stdin the copied text. It falls back to $TMUX_PANE when called
// with no argument, which is how the shim (below) reuses it — a process started
// inside a pane inherits that variable, and unlike anything a tmux hook can
// tell us it names the pane that actually copied.
//
// stderr goes to a log — the one failure the daemon can never record itself is
// "the request never arrived", and copy-mode must keep working regardless
// (|| true).
func clipboardScript(tokenPath, baseURL, logPath string) string {
	return strings.Join([]string{
		"#!/bin/sh",
		"# Written by ccmuxd at startup — do not edit. Pipes tmux copies to the",
		"# daemon so attached lenses mirror them to their OS clipboards.",
		"#",
		"# Exit status depends on WHO called. With a pane in $1 the caller is a tmux",
		"# copy binding, which must never break copy-mode over a daemon that is down:",
		"# always exit 0. Without one the caller is the shim standing in for xclip,",
		"# and its exit status is the copying app's ONLY failure signal — a 413, a",
		"# 401 after a daemon restart, or a refused connection has to travel back, or",
		"# the app reports a copy that never reached the clipboard and the user pastes",
		"# whatever was there before.",
		"# Caller is told apart by whether an argument was PASSED, not by whether it",
		"# is empty: a binding whose #{pane_id} came out blank is still a binding,",
		"# and must not start failing.",
		"strict=",
		`if [ $# -gt 0 ]; then`,
		`  pane="$1"`,
		"else",
		`  pane="$TMUX_PANE"`,
		"  strict=1",
		"fi",
		`log() { printf '%s ccmux-copy: %s\n' "$(date -u +%Y-%m-%dT%H:%M:%SZ)" "$1" >>` + fmt.Sprintf("%q; }", logPath),
		`if [ -z "$pane" ]; then`,
		`  log "no pane id (\$TMUX_PANE unset) — copy dropped"`,
		`  [ -z "$strict" ]`,
		"  exit",
		"fi",
		"if curl -fsS -m 2 \\",
		`  -H "X-Ccmux-Pane: $pane" \`,
		fmt.Sprintf(`  -H "X-Ccmux-Clip: $(cat %q 2>/dev/null)" \`, tokenPath),
		fmt.Sprintf(`  --data-binary @- %q \`, baseURL+"/v1/clipboard"),
		fmt.Sprintf(`  >/dev/null 2>>%q; then`, logPath),
		"  exit 0",
		"fi",
		`log "post failed for pane $pane"`,
		`[ -z "$strict" ]`,
		"exit",
		"",
	}, "\n")
}

// clipboardShimScript renders the fake `xclip` / `wl-copy` that hosted panes get
// on their PATH. Claude Code picks a copy strategy per platform: on Linux it
// uses one of these tools when it finds one, and otherwise falls back to
// `tmux load-buffer` — text into a buffer beside the daemon that no lens ever
// sees. Providing the tool it looks for keeps the copy in the copying process,
// where $TMUX_PANE is exact.
//
// This shim is deliberately NARROW, and refuses loudly rather than pretending.
// It sits at the FRONT of every hosted pane's PATH, so it shadows a genuinely
// installed xclip for every command in the pane, not only Claude Code — and the
// one thing it must never do is accept a copy it did not actually perform.
//
//   - Text as arguments (`wl-copy hello`, `xclip file.txt`) is refused with a
//     message. Reading stdin instead would post an empty body and report
//     success; implementing those forms faithfully means reimplementing three
//     different tools. Nothing is lost by refusing: on a host with no clipboard
//     tool — the only host that gets a shim — those commands did not work
//     before either, they just failed as "command not found".
//   - A terminal on stdin is refused for the same reason, and because reading
//     one would swallow the user's keystrokes in a pane that looks hung.
//   - `-o` / `--output` is a READ (Claude Code pulls clipboard images that way).
//     There is no clipboard on this host to read, so exit non-zero and let the
//     caller take its own not-available path.
//   - PRIMARY is declined: X11 clients write CLIPBOARD and PRIMARY as separate
//     calls, and mirroring both would push every copy to the lens twice. It exits
//     0 because declining is correct, but it says so on stderr — a tool that
//     targets PRIMARY only would otherwise see success for a copy that was
//     dropped, the one outcome the rest of this shim refuses to produce.
func clipboardShimScript(helperPath string) string {
	return strings.Join([]string{
		"#!/bin/sh",
		shimMarker + " — written by ccmuxd at startup, do not edit. Stands in for",
		"# xclip/wl-copy so an app's copy reaches the lens clipboard, not a tmux",
		"# buffer. The marker above is how an upgrade tells its own file apart from",
		"# a real clipboard tool it must not overwrite.",
		`while [ $# -gt 0 ]; do`,
		`  case "$1" in`,
		`    -o|--output|-out) exit 1 ;;`,
		`    -p|--primary)`,
		`      printf 'ccmux: PRIMARY selection is not mirrored to the lens\n' >&2`,
		`      exit 0 ;;`,
		// -selection/-t and friends take a value; skip it, but decline PRIMARY.
		`    -selection|-sel|-s|-t|-target|--type)`,
		`      case "$2" in`,
		`        primary|PRIMARY)`,
		`          printf 'ccmux: PRIMARY selection is not mirrored to the lens\n' >&2`,
		`          exit 0 ;;`,
		`      esac`,
		`      shift 2 || shift ;;`,
		`    -*) shift ;;`,
		`    *)`,
		`      printf 'ccmux: clipboard shim takes text on stdin only (got %s)\n' "$1" >&2`,
		`      exit 1 ;;`,
		"  esac",
		"done",
		`if [ -t 0 ]; then`,
		`  printf 'ccmux: clipboard shim will not read from a terminal\n' >&2`,
		"  exit 1",
		"fi",
		fmt.Sprintf(`exec %q`, helperPath),
		"",
	}, "\n")
}

// renderTmuxConf substitutes the managed config's placeholders. Pure, split
// out for the test that pins "no placeholder survives to the tmux server".
func renderTmuxConf(conf, copyScript string) string {
	return strings.ReplaceAll(conf, "__CCMUX_COPY__", copyScript)
}
