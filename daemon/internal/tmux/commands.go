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

// SendKeys injects raw bytes into a pane via `send-keys -H` (hex), the in-band,
// echo-safe input path.
func (c *Client) SendKeys(pane string, data []byte) error {
	args := append([]string{"send-keys", "-H", "-t", pane}, HexKeys(data)...)
	_, err := c.Command(args...)
	return err
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
