package main

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// setupClipboardPipe writes the two artifacts the tmux copy bindings need into
// the (0700, per-user) runtime dir: a per-boot random token and the ccmux-copy
// helper script that pipes copied text to POST /v1/clipboard with it.
//
// The token turns "anything on this machine can write every lens's clipboard"
// into a per-USER capability: another account can reach the loopback port but
// cannot read this user's runtime dir. It deliberately does NOT try to exclude
// same-user processes — they can drive the tmux socket directly and fake a
// copy anyway, so a same-user boundary here would be theater.
func setupClipboardPipe(dir, baseURL string) (scriptPath, token string, err error) {
	raw := make([]byte, 16)
	if _, err := rand.Read(raw); err != nil {
		return "", "", fmt.Errorf("mint token: %w", err)
	}
	token = hex.EncodeToString(raw)
	tokenPath := filepath.Join(dir, "clipboard-token")
	if err := os.WriteFile(tokenPath, []byte(token), 0o600); err != nil {
		return "", "", fmt.Errorf("write token: %w", err)
	}
	scriptPath = filepath.Join(dir, "ccmux-copy")
	script := clipboardScript(tokenPath, baseURL, filepath.Join(dir, "clipboard.log"))
	if err := os.WriteFile(scriptPath, []byte(script), 0o700); err != nil {
		return "", "", fmt.Errorf("write script: %w", err)
	}
	return scriptPath, token, nil
}

// clipboardScript renders the helper the tmux bindings invoke: $1 is the tmux
// pane id, stdin the copied text. stderr goes to a log — the one failure the
// daemon can never record itself is "the request never arrived", and copy-mode
// must keep working regardless (|| true).
func clipboardScript(tokenPath, baseURL, logPath string) string {
	return strings.Join([]string{
		"#!/bin/sh",
		"# Written by ccmuxd at startup — do not edit. Pipes tmux copy-mode text",
		"# to the daemon so attached lenses mirror it to their OS clipboards.",
		`curl -fsS -m 2 \`,
		`  -H "X-Ccmux-Pane: $1" \`,
		fmt.Sprintf(`  -H "X-Ccmux-Clip: $(cat %q 2>/dev/null)" \`, tokenPath),
		fmt.Sprintf(`  --data-binary @- %q \`, baseURL+"/v1/clipboard"),
		fmt.Sprintf(`  >/dev/null 2>>%q || true`, logPath),
		"",
	}, "\n")
}

// renderTmuxConf substitutes the managed config's placeholders. Pure, split
// out for the test that pins "no placeholder survives to the tmux server".
func renderTmuxConf(conf, copyScript string) string {
	return strings.ReplaceAll(conf, "__CCMUX_COPY__", copyScript)
}
