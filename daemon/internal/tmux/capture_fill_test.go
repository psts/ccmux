package tmux

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

// TestCaptureScreen_ReplaysColouredBlanks uses a second tmux pane as the lens
// emulator: a snapshot fed into it must reproduce the source screen cell for
// cell. Without -N capture-pane trims trailing spaces even when they carry a
// background, so a fullscreen program's painted background (opencode's black)
// came back as the lens's default colour wherever a row ended in blanks.
func TestCaptureScreen_ReplaysColouredBlanks(t *testing.T) {
	assertReplays(t, `printf '\033[44m\033[2J\033[H  hi\033[3;5Hthere\033[0m\033[4;1Hplain'; sleep 30`)
}

// TestCaptureScreen_ReplaysColouredBadge is the other half: a row whose last
// written cell is coloured, with nothing written after it, must not have that
// colour spread to the row's end (a menu highlight, git's whitespace marker).
func TestCaptureScreen_ReplaysColouredBadge(t *testing.T) {
	assertReplays(t, `printf 'x\033[44m ok \033[0m\r\nnext\033[41mR\033[0m'; sleep 30`)
}

func assertReplays(t *testing.T, paint string) {
	t.Helper()
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux not installed")
	}
	const socket = "ccmux-filltest"
	tmuxRun(t, socket, "kill-server")
	t.Cleanup(func() { tmuxRun(t, socket, "kill-server") })

	newSession(t, socket, "src", paint)
	time.Sleep(300 * time.Millisecond)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	c, err := Dial(ctx, socket, "src", newCollectHandler())
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer c.Close()
	rows, _, err := c.CaptureScreen("src")
	if err != nil {
		t.Fatalf("capture: %v", err)
	}

	frame := filepath.Join(t.TempDir(), "frame")
	if err := os.WriteFile(frame, append([]byte("\x1b[H\x1b[2J"), rows...), 0o600); err != nil {
		t.Fatal(err)
	}
	newSession(t, socket, "dst", "stty -onlcr; cat "+frame+"; sleep 30")
	time.Sleep(300 * time.Millisecond)

	want, got := cellsOf(t, socket, "src"), cellsOf(t, socket, "dst")
	if got != want {
		t.Fatalf("replayed screen differs from source\nsource:\n%q\nreplay:\n%q", want, got)
	}
}

func newSession(t *testing.T, socket, name, cmd string) {
	t.Helper()
	if err := exec.Command("tmux", "-L", socket, "-f", "/dev/null", "new-session", "-d",
		"-s", name, "-x", "20", "-y", "4", "sh", "-c", cmd).Run(); err != nil {
		t.Fatalf("new-session %s: %v", name, err)
	}
}

// cellsOf is every cell of a pane with its attributes, trailing blanks
// included (-N), so a background lost past the last character shows up.
func cellsOf(t *testing.T, socket, pane string) string {
	t.Helper()
	out, err := exec.Command("tmux", "-L", socket, "capture-pane", "-e", "-p", "-N", "-t", pane).Output()
	if err != nil {
		t.Fatalf("capture %s: %v", pane, err)
	}
	return string(out)
}
