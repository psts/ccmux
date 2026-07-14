// Package autoconfirm makes a Claude Code launch hands-free by pressing Enter on
// its two blocking startup prompts (trust-this-folder and load-development-
// channels) so a spawned session starts without a human attaching. Ported from
// TerminalStore.autoConfirmStartupPrompts / isStartupConfirmPrompt.
package autoconfirm

import (
	"context"
	"strings"
	"time"
)

// PaneIO is the slice of a session controller autoconfirm needs.
type PaneIO interface {
	CaptureText(paneID string) ([]byte, error)
	SendInput(paneID string, data []byte) error
}

const (
	// A spawned claude can take tens of seconds to render its first prompt.
	watchTimeout = 120 * time.Second
	pollInterval = 250 * time.Millisecond
	postEnterGap = 2 * time.Second
	// Up to two prompts (trust + dev-channels); a third for margin.
	maxEnters = 3
)

// IsStartupPrompt reports whether pane text is one of claude's startup
// confirmation prompts. claude's TUI positions words with cursor moves, leaving
// gaps that are NUL or space depending on the capture path, so we compact out
// NUL/space/newline and match the jammed phrase. Matching a spaced phrase would
// silently never fire.
func IsStartupPrompt(text string) bool {
	r := strings.NewReplacer("\x00", "", " ", "", "\n", "", "\t", "", "\r", "")
	compact := r.Replace(text)
	return strings.Contains(compact, "localdevelopment") || strings.Contains(compact, "trustthisfolder")
}

// Watch polls a freshly-spawned pane and presses Enter for each startup prompt
// it shows, then returns. Bounded so it never watches a pane forever, and it
// matches each prompt's SPECIFIC text (never a later tool-permission prompt).
// Plain CR (0x0D) confirms even under claude's kitty keyboard protocol.
func Watch(ctx context.Context, io PaneIO, paneID string) {
	deadline := time.Now().Add(watchTimeout)
	enters := 0
	for enters < maxEnters && time.Now().Before(deadline) {
		if ctx.Err() != nil {
			return
		}
		text, err := io.CaptureText(paneID)
		if err == nil && IsStartupPrompt(string(text)) {
			if io.SendInput(paneID, []byte{0x0d}) != nil {
				return
			}
			enters++
			if !sleepCtx(ctx, postEnterGap) {
				return
			}
			continue
		}
		if !sleepCtx(ctx, pollInterval) {
			return
		}
	}
}

// sleepCtx sleeps for d, returning false if ctx is cancelled first.
func sleepCtx(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}
