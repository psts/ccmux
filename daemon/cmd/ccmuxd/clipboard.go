package main

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
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
// ~/.local/bin — and reports whether the user's own shell actually resolves
// them. Both halves matter:
//
// Placement cannot go through a shell rc file. The first cut prepended the dir
// from the zsh ZDOTDIR proxy, which does nothing on a bash host, and handing
// PATH to tmux with `new-session -e` does not survive either: measured in
// containers, a Debian login shell drops an injected PATH via /etc/profile
// while an Ubuntu one keeps it. Installing into a directory the profile already
// puts on PATH is the one mechanism that does not care which shell you run —
// verified as first on PATH for a real user on both distros.
//
// No-op off Linux: macOS copies with pbcopy, which already writes the machine
// the lens runs on. Only a headless Linux host has nothing for Claude Code to
// find and falls back to a tmux buffer.
func installClipboardShims(helperPath string) bool {
	if runtime.GOOS != "linux" {
		return false
	}
	self, err := resolveSelf()
	if err != nil {
		log.Printf("clipboard shim: cannot locate the ccmuxd binary (%v) — app-made copies will land in a tmux buffer", err)
		return false
	}
	binDir := filepath.Dir(self)
	body := []byte(clipboardShimScript(helperPath))
	for _, name := range shimNames {
		path := filepath.Join(binDir, name)
		if !ownedByUs(path) {
			log.Printf("clipboard shim: %s already exists and is not ours — leaving it alone", path)
			continue
		}
		if err := os.WriteFile(path, body, 0o755); err != nil {
			log.Printf("clipboard shim: write %s: %v", path, err)
		}
	}
	return shimResolves(binDir)
}

// ownedByUs reports whether path is absent or is a shim this daemon wrote.
// A real clipboard tool sitting there must never be overwritten.
func ownedByUs(path string) bool {
	b, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return true
	}
	if err != nil {
		return false
	}
	return strings.Contains(string(b), shimMarker)
}

// shimResolves asks the user's OWN login shell what `xclip` resolves to, rather
// than assuming a directory is on PATH. That is the shell-agnostic check: the
// answer comes from whatever startup files that shell actually reads, so a bash
// host, a zsh host, and a distro with an opinionated /etc/profile all report
// honestly instead of being guessed at.
func shimResolves(binDir string) bool {
	shell := os.Getenv("SHELL")
	if shell == "" {
		shell = "/bin/sh"
	}
	out, err := exec.Command(shell, "-lc", "command -v xclip").Output()
	if err != nil {
		log.Printf("clipboard shim: %s could not resolve xclip (%v) — not claiming a display; app-made copies will land in a tmux buffer", shell, err)
		return false
	}
	got := strings.TrimSpace(string(out))
	if got != filepath.Join(binDir, "xclip") {
		log.Printf("clipboard shim: %s resolves xclip to %q, not our shim — not claiming a display; put %s on PATH to enable app-made copies", shell, got, binDir)
		return false
	}
	return true
}

// removeClipboardShims deletes the shims this daemon installed, leaving any real
// tool that happens to share the name. Called from uninstall.
func removeClipboardShims() {
	self, err := resolveSelf()
	if err != nil {
		return
	}
	for _, name := range shimNames {
		path := filepath.Join(filepath.Dir(self), name)
		if b, err := os.ReadFile(path); err == nil && strings.Contains(string(b), shimMarker) {
			_ = os.Remove(path)
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
//     calls, and mirroring both would push every copy to the lens twice.
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
		`    -p|--primary) exit 0 ;;`,
		// -selection/-t and friends take a value; skip it, but decline PRIMARY.
		`    -selection|-sel|-s|-t|-target|--type)`,
		`      case "$2" in primary|PRIMARY) exit 0 ;; esac`,
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
