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
//
// Newlines are rejected for a sharper reason: control mode is LINE-based, so a
// newline inside an argument ends the command and everything after it runs as
// the NEXT command — quoting does not help, because the framing happens before
// tmux parses quotes. Verified against tmux 3.4: an arg of
// "a\nrename-session -t it PWNED" renamed the session, single quotes and all.
// It is reachable — SpawnWindow puts cwd and env values straight into args
// (session/controller.go), a Linux directory name may legally contain a
// newline, and dev_command is user-set. There is nothing to escape it to, so
// the only correct answer is to refuse and let the caller fail loudly.
func formatCommand(args []string) (string, error) {
	parts := make([]string, len(args))
	for i, a := range args {
		if strings.ContainsRune(a, '\'') {
			return "", fmt.Errorf("tmux: arg %q contains a single quote (unsupported)", a)
		}
		if strings.ContainsAny(a, "\n\r") {
			return "", fmt.Errorf("tmux: arg %q contains a newline (would inject a second command)", a)
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

// sendKeysPrefixArgs is how many args precede the hex bytes in each command
// ("send-keys", "-H", "-t", pane), so a chunk's byte count is len(args) minus
// this. Named rather than a literal 4 because SendKeys's progress accounting
// silently goes wrong if the prefix ever changes.
const sendKeysPrefixArgs = 4

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

// SendKeys injects raw bytes into a pane and blocks until they are in tmux,
// returning the delivery error. Unchanged contract: every caller that needs
// "the bytes are there now" — startup commands, autoconfirm, the harness —
// keeps using this.
//
// It goes through the pane's queue rather than around it. A second path
// straight to the wire would race the queued one, and then neither ordering
// would hold.
func (c *Client) SendKeys(pane string, data []byte) error {
	errc := make(chan error, 1)
	c.paneSender(pane).submit(sendJob{
		data: data,
		done: func(err error) { errc <- err },
	})
	return <-errc
}

// SendKeysAsync queues bytes and returns immediately; done, if set, is called
// with the delivery result on the sender's goroutine.
//
// This exists for api.applyInput, which runs on the attach read goroutine —
// the one that also dispatches resize, repaint and focus, and that owns the
// websocket read deadline. Blocking it for the ~3s of a 1 MB paste froze every
// other pane on that lens. Input order survives returning early: the queue is
// FIFO per pane for a given submitting goroutine, which is what the blocking
// used to supply and what the mutex it replaced never did.
//
// data is copied. The queue outlives the caller's stack, and every caller
// today happens to pass a fresh slice — "happens to" is not something a
// background worker should depend on.
func (c *Client) SendKeysAsync(pane string, data []byte, done func(error)) {
	cp := make([]byte, len(data))
	copy(cp, data)
	c.paneSender(pane).submit(sendJob{data: cp, done: done})
}

// PartialSendError reports a send that failed partway, naming how much reached
// the pane. Typed rather than a formatted string because the lens tells the
// user "your paste was cut short after N of M bytes", and recovering those
// numbers by parsing an error message is the kind of thing that silently stops
// working when someone rewords it.
//
// Sent may be 0: the first chunk can fail, in which case nothing landed.
type PartialSendError struct {
	Pane        string
	Sent, Total int
	Err         error
}

func (e *PartialSendError) Error() string {
	return fmt.Sprintf("send-keys to %s: %d of %d bytes delivered: %v",
		e.Pane, e.Sent, e.Total, e.Err)
}

func (e *PartialSendError) Unwrap() error { return e.Err }

// sendKeysNow performs one send: split into `send-keys -H` (hex) commands, the
// in-band echo-safe input path, and issue them in order. Called only from a
// pane's sender goroutine, which is what serializes it — one job is processed
// whole, so another lens's keystroke lands before or after a paste, never
// inside it.
//
// Splitting trades the old failure for a smaller one. A mid-send error leaves
// the earlier chunks in the pane, where an over-limit send used to deliver
// nothing at all. The caller learns how much landed but not that the pane is
// now holding a truncated prefix; api.applyInput only logs it. That is worse
// to diagnose than the old all-or-nothing, and better than a 19 kB paste never
// working.
func (c *Client) sendKeysNow(pane string, data []byte) error {
	sent := 0
	for _, args := range sendKeysCommands(pane, data) {
		if _, err := c.Command(args...); err != nil {
			// Say how much landed. api.applyInput only logs this, so without
			// the offset the log records that a paste failed but not that the
			// pane is now holding a truncated prefix of it.
			return &PartialSendError{Pane: pane, Sent: sent, Total: len(data), Err: err}
		}
		sent += len(args) - sendKeysPrefixArgs
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
