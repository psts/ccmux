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
// (`capture-pane -e -p`), joined with newlines. Bytes are raw — command replies
// are not octal-escaped — so lenses can feed the result straight into their
// emulator to seed a fresh attach.
func (c *Client) CapturePane(pane string, historyLines int) ([]byte, error) {
	args := []string{"capture-pane", "-e", "-p", "-t", pane}
	if historyLines > 0 {
		args = append(args, "-S", "-"+strconv.Itoa(historyLines))
	}
	lines, err := c.Command(args...)
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
