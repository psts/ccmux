package tmux

import (
	"context"
	"os/exec"
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

func tmuxRun(t *testing.T, socket string, args ...string) {
	t.Helper()
	_ = exec.Command("tmux", append([]string{"-L", socket}, args...)...).Run()
}
