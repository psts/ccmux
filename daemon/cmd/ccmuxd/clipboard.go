package main

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

// setupClipboardPipe writes the artifacts the copy paths need into the (0700,
// per-user) runtime dir: a per-boot random token, the ccmux-copy helper the tmux
// copy bindings pipe into, and — on Linux — the xclip/wl-copy shim directory
// that hosted panes get on their PATH. shimDir is "" where no shim is written.
//
// The token turns "anything on this machine can write every lens's clipboard"
// into a per-USER capability: another account can reach the loopback port but
// cannot read this user's runtime dir. It deliberately does NOT try to exclude
// same-user processes — they can drive the tmux socket directly and fake a
// copy anyway, so a same-user boundary here would be theater.
func setupClipboardPipe(dir, baseURL string) (scriptPath, shimDir, token string, err error) {
	raw := make([]byte, 16)
	if _, err := rand.Read(raw); err != nil {
		return "", "", "", fmt.Errorf("mint token: %w", err)
	}
	token = hex.EncodeToString(raw)
	tokenPath := filepath.Join(dir, "clipboard-token")
	if err := os.WriteFile(tokenPath, []byte(token), 0o600); err != nil {
		return "", "", "", fmt.Errorf("write token: %w", err)
	}
	scriptPath = filepath.Join(dir, "ccmux-copy")
	script := clipboardScript(tokenPath, baseURL, filepath.Join(dir, "clipboard.log"))
	if err := os.WriteFile(scriptPath, []byte(script), 0o700); err != nil {
		return "", "", "", fmt.Errorf("write script: %w", err)
	}
	// The shim is an enhancement over the tmux-buffer fallback; the pipe above is
	// the feature. Failing the whole call here would take out copy-mode mirroring
	// — which was working — over a directory that could not be created.
	shimDir, err = writeClipboardShims(dir, scriptPath)
	if err != nil {
		log.Printf("clipboard shim not installed (%v) — copy-mode still mirrors, but an app's own copy will land in a tmux buffer", err)
		shimDir = ""
	}
	return scriptPath, shimDir, token, nil
}

// writeClipboardShims installs the fake clipboard tools, and is a no-op off
// Linux. macOS needs nothing: Claude Code copies there with pbcopy, which
// already writes the clipboard of the machine the lens runs on. Windows/WSL
// likewise. Only a headless Linux host has no clipboard for Claude Code to find
// and falls back to a tmux buffer.
func writeClipboardShims(dir, helperPath string) (string, error) {
	if runtime.GOOS != "linux" {
		return "", nil
	}
	shimDir := filepath.Join(dir, "clipbin")
	if err := os.MkdirAll(shimDir, 0o700); err != nil {
		return "", fmt.Errorf("mkdir shim dir: %w", err)
	}
	shim := clipboardShimScript(helperPath)
	for _, name := range []string{"xclip", "wl-copy", "xsel"} {
		if err := os.WriteFile(filepath.Join(shimDir, name), []byte(shim), 0o700); err != nil {
			return "", fmt.Errorf("write %s shim: %w", name, err)
		}
	}
	return shimDir, nil
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
		"# Written by ccmuxd at startup — do not edit. Stands in for xclip/wl-copy",
		"# so an app's copy reaches the lens clipboard instead of a tmux buffer.",
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
