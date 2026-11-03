// Package autoconfirm makes a Claude Code launch hands-free by pressing Enter on
// its two blocking startup prompts (trust-this-folder and load-development-
// channels) so a spawned session starts without a human attaching. Ported from
// TerminalStore.autoConfirmStartupPrompts / isStartupConfirmPrompt.
package autoconfirm

import (
	"context"
	"log"
	"regexp"
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
	// The trust prompt's choice is one step below its "No, exit"; a few
	// more allow for a slow redraw, never an endless walk.
	maxDowns = 4
)

// IsStartupPrompt reports whether pane text is one of claude's startup
// confirmation prompts. claude's TUI positions words with cursor moves, leaving
// gaps that are NUL or space depending on the capture path, so we compact out
// NUL/space/newline and match the jammed phrase. Matching a spaced phrase would
// silently never fire.
func IsStartupPrompt(text string) bool { return isPrompt(compact(text)) }

func compact(text string) string {
	return strings.NewReplacer("\x00", "", " ", "", "\n", "", "\t", "", "\r", "").Replace(text)
}

func isPrompt(c string) bool {
	return strings.Contains(c, "localdevelopment") || strings.Contains(c, "trustthisfolder")
}

var (
	enter = []byte{0x0d}
	down  = []byte("\x1b[B")
)

// keyFor is the key a pane's screen asks for: nil when it shows no startup
// prompt, Down when the cursor sits on the trust prompt's "No, exit" (Claude
// Code puts it there for a folder it has never seen, such as a fresh
// worktree, and Enter would quit), else Enter.
func keyFor(text string) []byte {
	c := compact(text)
	switch {
	case !isPrompt(c):
		return nil
	case onNo.MatchString(c):
		return down
	}
	return enter
}

// onNo is the cursor on the trust prompt's "No, exit", numbered or not.
var onNo = regexp.MustCompile(`❯(\d+\.)?No,exit`)

// Watch polls a freshly-spawned pane and presses Enter for each startup prompt
// it shows (moving to "Yes" first where the cursor starts on "No"), then returns. Bounded so it never watches a pane forever, and it
// matches each prompt's SPECIFIC text (never a later tool-permission prompt).
// Plain CR (0x0D) confirms even under claude's kitty keyboard protocol.
func Watch(ctx context.Context, io PaneIO, paneID string) {
	deadline := time.Now().Add(watchTimeout)
	var w presses
	for w.enters < maxEnters && time.Now().Before(deadline) && ctx.Err() == nil {
		key := w.allow(screenKey(io, paneID))
		if key != nil {
			if err := io.SendInput(paneID, key); err != nil {
				log.Printf("autoconfirm: pane %s: could not answer its startup prompt: %v", paneID, err)
				return
			}
		}
		if !sleepCtx(ctx, w.count(key)) {
			return
		}
	}
	if w.enters == 0 && w.downs > 0 {
		// It walked off "No" but never confirmed: the session is likely still
		// sitting on the trust prompt, and nothing else will say so.
		log.Printf("autoconfirm: pane %s: left its trust prompt unconfirmed after %d moves", paneID, w.downs)
	}
}

// screenKey reads the pane and names the key it asks for, nil for none.
func screenKey(io PaneIO, paneID string) []byte {
	text, err := io.CaptureText(paneID)
	if err != nil {
		return nil
	}
	return keyFor(string(text))
}

// presses counts the keys Watch sent, which bound it.
type presses struct{ enters, downs int }

// allow drops a Down past maxDowns: the walk off "No" is bounded, but the
// watch goes on until the prompt is confirmed or the deadline passes.
func (p *presses) allow(key []byte) []byte {
	if string(key) == string(down) && p.downs >= maxDowns {
		return nil
	}
	return key
}

// count records key and returns how long to wait before looking again: an
// Enter gets time for the next screen to draw.
func (p *presses) count(key []byte) time.Duration {
	switch {
	case key == nil:
	case string(key) == string(down):
		p.downs++
	default:
		p.enters++
		return postEnterGap
	}
	return pollInterval
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
