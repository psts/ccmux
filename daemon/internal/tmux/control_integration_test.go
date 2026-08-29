package tmux

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// collectHandler records output bytes and notification kinds for assertions.
type collectHandler struct {
	mu     sync.Mutex
	output map[string][]byte
	kinds  []string
}

func newCollectHandler() *collectHandler {
	return &collectHandler{output: map[string][]byte{}}
}

func (h *collectHandler) OnOutput(pane string, data []byte) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.output[pane] = append(h.output[pane], data...)
}

func (h *collectHandler) OnNotification(kind, _ string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.kinds = append(h.kinds, kind)
}

func (h *collectHandler) outputContains(sub string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, b := range h.output {
		if strings.Contains(string(b), sub) {
			return true
		}
	}
	return false
}

// TestControlClient_EndToEnd exercises the full transport against a real tmux
// server: dial control mode, run a command, inject keys, capture, and receive
// live %output. Skips if tmux is unavailable.
func TestControlClient_EndToEnd(t *testing.T) {
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux not installed")
	}
	const socket = "ccmux-itest"
	tmuxRun(t, socket, "kill-server") // ignore error
	t.Cleanup(func() { tmuxRun(t, socket, "kill-server") })

	if err := exec.Command("tmux", "-L", socket, "-f", "/dev/null",
		"new-session", "-d", "-s", "it", "-x", "80", "-y", "24").Run(); err != nil {
		t.Fatalf("new-session: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	h := newCollectHandler()
	c, err := Dial(ctx, socket, "it", h)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer c.Close()

	// Command reply round-trips.
	lines, err := c.Command("display-message", "-p", "ok-#{session_name}")
	if err != nil {
		t.Fatalf("display-message: %v", err)
	}
	if len(lines) != 1 || lines[0] != "ok-it" {
		t.Fatalf("display-message reply = %v, want [ok-it]", lines)
	}

	// Inject a marker via send-keys (hex path) and let it echo as %output.
	marker := "ZZ_MARKER_9137"
	if err := c.SendKeys("it", []byte("printf "+marker+"\n")); err != nil {
		t.Fatalf("send-keys: %v", err)
	}

	// Wait for the marker to appear in captured pane contents.
	deadline := time.Now().Add(3 * time.Second)
	var captured string
	for time.Now().Before(deadline) {
		b, err := c.CapturePane("it", 0)
		if err != nil {
			t.Fatalf("capture: %v", err)
		}
		captured = string(b)
		if strings.Contains(captured, marker) {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if !strings.Contains(captured, marker) {
		t.Fatalf("marker %q not found in capture:\n%s", marker, captured)
	}

	// The keystrokes should also have arrived as live %output.
	if !h.outputContains(marker) {
		t.Errorf("marker %q not seen in live %%output stream", marker)
	}
}

// TestCapturePane_AppendsCursorRestore pins the seed-cursor fix: a visible-screen
// capture must end with a CUP escape at tmux's real cursor, so a lens that feeds the
// (blank-line-padded) capture doesn't strand the cursor far below the prompt.
func TestCapturePane_AppendsCursorRestore(t *testing.T) {
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux not installed")
	}
	const socket = "ccmux-curtest"
	tmuxRun(t, socket, "kill-server")
	t.Cleanup(func() { tmuxRun(t, socket, "kill-server") })

	if err := exec.Command("tmux", "-L", socket, "-f", "/dev/null",
		"new-session", "-d", "-s", "it", "-x", "80", "-y", "24").Run(); err != nil {
		t.Fatalf("new-session: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	c, err := Dial(ctx, socket, "it", newCollectHandler())
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer c.Close()

	// Type a token so the cursor sits mid-line, then let the pane settle.
	if err := c.SendKeys("it", []byte("echo hi")); err != nil {
		t.Fatalf("send-keys: %v", err)
	}
	time.Sleep(300 * time.Millisecond)

	x, y, err := c.CursorPosition("it")
	if err != nil {
		t.Fatalf("cursor position: %v", err)
	}
	b, err := c.CapturePane("it", 0)
	if err != nil {
		t.Fatalf("capture: %v", err)
	}
	wantCUP := fmt.Sprintf("\x1b[%d;%dH", y+1, x+1) // CUP is 1-indexed
	if !strings.HasSuffix(string(b), wantCUP) {
		t.Fatalf("capture should end with cursor restore %q; tail:\n%q", wantCUP, tailStr(b, 16))
	}

	// A history capture (historyLines > 0) must NOT append a CUP — its row numbers
	// don't map to the visible screen.
	hist, err := c.CapturePane("it", 5)
	if err != nil {
		t.Fatalf("capture history: %v", err)
	}
	if strings.HasSuffix(string(hist), "H") && strings.Contains(string(hist), "\x1b[") &&
		strings.HasSuffix(string(hist), wantCUP) {
		t.Errorf("history capture unexpectedly ends with a cursor restore")
	}
}

func tailStr(b []byte, n int) string {
	if len(b) > n {
		b = b[len(b)-n:]
	}
	return string(b)
}

func tmuxRun(t *testing.T, socket string, args ...string) {
	t.Helper()
	_ = exec.Command("tmux", append([]string{"-L", socket}, args...)...).Run()
}

// TestControlClient_LargePaste is the regression for the bug this chunking
// exists for: before sendKeysCommands split it, a 19 kB paste became a single
// `send-keys -H` with 19000 arguments, tmux answered "parse error: yacc stack
// overflow", and NOT ONE byte reached the pane. Only a live server proves the
// fix, since the limit lives in tmux's parser.
//
// The pane runs cat in raw mode: cooked mode holds a line until a newline and
// caps it at 4096, which would measure the tty rather than the transport.
func TestControlClient_LargePaste(t *testing.T) {
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux not installed")
	}
	const socket = "ccmux-paste-itest"
	sink := filepath.Join(t.TempDir(), "sink")
	tmuxRun(t, socket, "kill-server") // ignore error
	t.Cleanup(func() { tmuxRun(t, socket, "kill-server") })

	if err := exec.Command("tmux", "-L", socket, "-f", "/dev/null",
		"new-session", "-d", "-s", "it", "-x", "80", "-y", "24",
		"sh -c 'stty raw -echo; exec cat > "+sink+"'").Run(); err != nil {
		t.Fatalf("new-session: %v", err)
	}

	c, err := Dial(t.Context(), socket, "it", newCollectHandler())
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer c.Close()

	// 19 kB, the reported size. A period-26 pattern catches a dropped,
	// duplicated or reordered chunk, not just a wrong total.
	want := make([]byte, 19*1024)
	for i := range want {
		want[i] = byte('a' + i%26)
	}
	if err := c.SendKeys("it", want); err != nil {
		t.Fatalf("SendKeys(%d bytes): %v", len(want), err)
	}

	var got []byte
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if got, _ = os.ReadFile(sink); len(got) >= len(want) {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if len(got) != len(want) {
		t.Fatalf("pane received %d bytes, want %d", len(got), len(want))
	}
	if !bytes.Equal(got, want) {
		for i := range want {
			if got[i] != want[i] {
				t.Fatalf("byte %d differs: got %q want %q", i, got[i], want[i])
			}
		}
	}
}
