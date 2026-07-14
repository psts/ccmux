// Command spike-lens is a passthrough control-mode "lens": it forwards a tmux
// pane's raw %output to stdout and forwards stdin keystrokes to the pane via
// send-keys -H. This is the exact byte path the real SwiftTerm / xterm.js lenses
// will use, so it's the vehicle for the S1 visual rendering gate: run Claude
// Code in a managed tmux session, view it through this lens, and eyeball table
// alignment, colors, Shift+Enter, ESC latency, and spinner smoothness.
//
// Must run under a raw, no-echo terminal (see scripts/s1-render-check.sh).
// Press Ctrl-] to detach.
package main

import (
	"bufio"
	"context"
	"flag"
	"fmt"
	"os"
	"strings"
	"sync"

	"ccmux.dev/ccmuxd/internal/tmux"
)

const detachKey = 0x1d // Ctrl-]

type lens struct {
	mu  sync.Mutex
	out *bufio.Writer
}

func (l *lens) OnOutput(_ string, data []byte) {
	l.mu.Lock()
	l.out.Write(data)
	l.out.Flush()
	l.mu.Unlock()
}

func (l *lens) OnNotification(kind, _ string) {
	if kind == "exit" {
		fmt.Fprint(os.Stderr, "\r\n[lens] tmux exited\r\n")
	}
}

func main() {
	socket := flag.String("L", "ccmux-s1", "tmux socket")
	session := flag.String("t", "s1", "session")
	cols := flag.Int("cols", 80, "local terminal columns")
	rows := flag.Int("rows", 24, "local terminal rows")
	flag.Parse()

	l := &lens{out: bufio.NewWriter(os.Stdout)}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	c, err := tmux.Dial(ctx, *socket, *session, l)
	if err != nil {
		fmt.Fprintf(os.Stderr, "dial: %v\r\n", err)
		os.Exit(1)
	}
	defer c.Close()

	pane := firstPane(c, *session)
	// Force Claude to full-repaint into %output: clear locally, then nudge the
	// window size so the inner app redraws from scratch (more faithful than
	// pasting a capture-pane snapshot).
	l.out.WriteString("\x1b[2J\x1b[H")
	l.out.Flush()
	c.ResizeWindow(pane, *cols, *rows-1)
	c.ResizeWindow(pane, *cols, *rows)

	go forwardStdin(c, pane, cancel)

	select {
	case <-c.Done():
	case <-ctx.Done():
	}
	fmt.Fprint(os.Stderr, "\r\n[lens] detached\r\n")
}

// firstPane returns the first pane id (%N) of the session's active window.
func firstPane(c *tmux.Client, session string) string {
	lines, err := c.Command("list-panes", "-t", session, "-F", "#{pane_id}")
	if err != nil || len(lines) == 0 {
		return session // fall back to session target
	}
	return strings.TrimSpace(lines[0])
}

// forwardStdin reads raw keystrokes and injects them into the pane, honoring the
// Ctrl-] detach key.
func forwardStdin(c *tmux.Client, pane string, cancel context.CancelFunc) {
	buf := make([]byte, 4096)
	for {
		n, err := os.Stdin.Read(buf)
		if n > 0 {
			chunk := buf[:n]
			if i := indexByte(chunk, detachKey); i >= 0 {
				if i > 0 {
					c.SendKeys(pane, chunk[:i])
				}
				cancel()
				return
			}
			if err := c.SendKeys(pane, chunk); err != nil {
				cancel()
				return
			}
		}
		if err != nil {
			cancel()
			return
		}
	}
}

func indexByte(b []byte, target byte) int {
	for i, c := range b {
		if c == target {
			return i
		}
	}
	return -1
}
