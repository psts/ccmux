package tmux

import (
	"fmt"
	"strconv"
	"strings"
)

// formatCommand renders command args into a single control-mode command line.
// tmux parses this line with shell-like quoting. We single-quote any arg that
// needs it (single quotes are fully literal in tmux — no #{} format expansion,
// unlike double quotes), which is exactly what we want for paths and data.
//
// tmux single-quote syntax cannot represent an embedded single quote, so we
// reject that case rather than emit something that would mis-parse. Callers that
// need arbitrary bytes in a pane use SendKeys (hex), not command args.
func formatCommand(args []string) (string, error) {
	parts := make([]string, len(args))
	for i, a := range args {
		if strings.ContainsRune(a, '\'') {
			return "", fmt.Errorf("tmux: arg %q contains a single quote (unsupported)", a)
		}
		if a == "" || strings.ContainsAny(a, " \t\"\\$#;{}") {
			parts[i] = "'" + a + "'"
		} else {
			parts[i] = a
		}
	}
	return strings.Join(parts, " "), nil
}

// maxKeysPerCommand caps how many bytes one `send-keys -H` carries.
//
// HexKeys renders ONE ARGUMENT PER BYTE, and tmux parses a command line with a
// yacc grammar whose stack is 10000 deep. Past ~9990 arguments the command dies
// with "parse error: yacc stack overflow" and NOTHING reaches the pane — not a
// truncated paste, an empty one. Measured against a live tmux 3.4 server: 9990
// arguments parse, 9995 do not. That is what silently ate a 19 kB paste, since
// api.applyInput logs the send error and drops the input.
//
// 1024 is not just "safely under" that limit, it is near the fast end of a
// sharply non-linear curve. Per-chunk cost grows roughly QUADRATICALLY with
// chunk size, so fewer, larger commands are much slower. Measured on tmux 3.4,
// 1 MB through this path:
//
//	chunk  256 -> 2.2s    chunk 2048 -> 5.4s
//	chunk  512 -> 2.5s    chunk 4096 -> 15.4s
//	chunk 1024 -> 3.0s    chunk 8192 -> 26.9s
//
// 256..1024 is the flat region; 1024 sits in it while issuing a quarter of the
// commands 256 would. Raising this is a pessimization, not an optimization.
const maxKeysPerCommand = 1024

// sendKeysCommands splits data into the ordered `send-keys -H` commands that
// carry it. Pure, so the chunk boundaries are testable without a tmux server.
//
// Splitting mid-escape-sequence is safe: the pane is a byte stream and the
// program on the far side reassembles, exactly as it does for a paste that
// arrives in two reads.
//
// Empty data yields no commands. `send-keys -H` with no keys is a no-op that
// would still cost a round trip.
func sendKeysCommands(pane string, data []byte) [][]string {
	var cmds [][]string
	for len(data) > 0 {
		n := min(len(data), maxKeysPerCommand)
		cmds = append(cmds, append([]string{"send-keys", "-H", "-t", pane}, HexKeys(data[:n])...))
		data = data[n:]
	}
	return cmds
}

// SendKeys injects raw bytes into a pane via `send-keys -H` (hex), the in-band,
// echo-safe input path. Oversized input is split (see maxKeysPerCommand).
//
// The split runs under that pane's lock so a paste stays contiguous: panes are
// shared between lenses, so without it another lens's keystroke could land
// between two chunks and appear in the middle of the pasted text. The lock is
// per pane, not per connection, because a big send is slow enough (seconds)
// that a shared one would freeze typing everywhere else in the workspace.
//
// Splitting trades the old failure for a smaller one. A mid-send error now
// leaves the earlier chunks in the pane, where an over-limit send used to
// deliver nothing at all. The caller learns only that it failed, not where, and
// api.applyInput just logs it — so a connection that dies mid-paste shows up as
// a truncated paste with no message. That is worse to diagnose than the old
// all-or-nothing, and better than a 19 kB paste never working.
func (c *Client) SendKeys(pane string, data []byte) error {
	lock := c.paneSend(pane)
	lock.Lock()
	defer lock.Unlock()
	for _, args := range sendKeysCommands(pane, data) {
		if _, err := c.Command(args...); err != nil {
			return err
		}
	}
	return nil
}

// ResizeWindow sets a window's size. Requires window-size manual on the session
// for the size to stick regardless of client dimensions.
func (c *Client) ResizeWindow(window string, cols, rows int) error {
	_, err := c.Command("resize-window", "-t", window, "-x", strconv.Itoa(cols), "-y", strconv.Itoa(rows))
	return err
}

// CapturePane returns the current pane contents with escape sequences preserved
// (`capture-pane -e -p`). Lines are joined with CRLF, not LF: the result is fed
// raw into a lens emulator to seed a fresh attach, and a bare LF only moves the
// cursor down — without the CR each seeded line would start at the previous
// line's column (a staircase). Bytes are otherwise raw (command replies are not
// octal-escaped).
//
// For a visible-screen capture (historyLines == 0) it appends a cursor-restore
// (CUP) escape: capture-pane emits every pane row including the trailing blank
// ones, so feeding them leaves the emulator cursor at the bottom of the screen,
// not where tmux's cursor actually is. The trailing CUP snaps it back so typed
// input lands at the prompt rather than far below it.
func (c *Client) CapturePane(pane string, historyLines int) ([]byte, error) {
	args := []string{"capture-pane", "-e", "-p", "-t", pane}
	if historyLines > 0 {
		args = append(args, "-S", "-"+strconv.Itoa(historyLines))
	}
	lines, err := c.Command(args...)
	if err != nil {
		return nil, err
	}
	out := []byte(strings.Join(lines, "\r\n"))
	if historyLines == 0 {
		if x, y, err := c.CursorPosition(pane); err == nil {
			// CUP is 1-indexed; tmux cursor_x/_y are 0-indexed.
			out = append(out, []byte(fmt.Sprintf("\x1b[%d;%dH", y+1, x+1))...)
		}
	}
	return out, nil
}

// CursorPosition returns a pane's 0-indexed cursor column (x) and row (y),
// relative to the top of the visible screen.
func (c *Client) CursorPosition(pane string) (x, y int, err error) {
	lines, err := c.Command("display-message", "-p", "-t", pane, "#{cursor_x} #{cursor_y}")
	if err != nil {
		return 0, 0, err
	}
	if len(lines) == 0 {
		return 0, 0, fmt.Errorf("tmux: empty cursor position for pane %s", pane)
	}
	if _, err := fmt.Sscanf(strings.TrimSpace(lines[0]), "%d %d", &x, &y); err != nil {
		return 0, 0, fmt.Errorf("tmux: parse cursor position %q: %w", lines[0], err)
	}
	return x, y, nil
}

// CapturePlain returns a pane's visible contents as plain text (no escape
// sequences), for prompt/text matching.
func (c *Client) CapturePlain(pane string) ([]byte, error) {
	lines, err := c.Command("capture-pane", "-p", "-t", pane)
	if err != nil {
		return nil, err
	}
	return []byte(strings.Join(lines, "\n")), nil
}

// SetOption sets a tmux option (global with -g, or targeted).
func (c *Client) SetOption(args ...string) error {
	_, err := c.Command(append([]string{"set-option"}, args...)...)
	return err
}
