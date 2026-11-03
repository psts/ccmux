package autoconfirm

import (
	"context"
	"sync"
	"testing"
	"time"
)

func TestIsStartupPrompt(t *testing.T) {
	yes := []string{
		"Loading development channels\n1. I am using this for local development",
		"local\x00development", // NUL-gapped, as claude's TUI can render
		"Is this a project you trust?\n1. Yes, I trust this folder",
		"trust  this   folder",
	}
	no := []string{
		"",
		"Do you want to allow this tool to run? 1. Yes 2. No",
		"just some normal output about a development server",
	}
	for _, s := range yes {
		if !IsStartupPrompt(s) {
			t.Errorf("IsStartupPrompt(%q) = false, want true", s)
		}
	}
	for _, s := range no {
		if IsStartupPrompt(s) {
			t.Errorf("IsStartupPrompt(%q) = true, want false", s)
		}
	}
}

// fakeIO returns a scripted sequence of captures and records sent input.
type fakeIO struct {
	mu       sync.Mutex
	captures []string
	idx      int
	sent     [][]byte
}

func (f *fakeIO) CaptureText(string) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.idx < len(f.captures) {
		s := f.captures[f.idx]
		f.idx++
		return []byte(s), nil
	}
	return []byte("idle prompt"), nil
}

func (f *fakeIO) SendInput(_ string, data []byte) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sent = append(f.sent, append([]byte(nil), data...))
	return nil
}

func TestWatch_PressesEnterOnPrompt(t *testing.T) {
	io := &fakeIO{captures: []string{
		"still starting up...",
		"Is this a project you trust?\n1. Yes, I trust this folder",
	}}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan struct{})
	go func() { Watch(ctx, io, "pane-1"); close(done) }()

	// Give it time to poll past the first capture and confirm the second.
	deadline := time.After(3 * time.Second)
	for {
		io.mu.Lock()
		n := len(io.sent)
		io.mu.Unlock()
		if n >= 1 {
			break
		}
		select {
		case <-deadline:
			t.Fatal("no Enter sent for startup prompt")
		case <-time.After(20 * time.Millisecond):
		}
	}
	io.mu.Lock()
	if len(io.sent[0]) != 1 || io.sent[0][0] != 0x0d {
		t.Errorf("sent %v, want [0x0d]", io.sent[0])
	}
	io.mu.Unlock()
	cancel()
	<-done
}

// The trust prompt of a folder Claude has never seen starts on "No, exit":
// Enter there quits. Watch moves down to "Yes" first, then confirms.
func TestKeyFor_TrustPromptOnNo(t *testing.T) {
	onNo := "Quick safety check: Is this a project you trust?\n ❯ No, exit\n   Yes, I trust this folder"
	onYes := "Quick safety check: Is this a project you trust?\n   No, exit\n ❯ Yes, I trust this folder"
	if got := keyFor(onNo); string(got) != string(down) {
		t.Fatalf("cursor on No: key %q, want Down", got)
	}
	if got := keyFor("Is this a project you trust?\n ❯ 2. No, exit\n   1. Yes, I trust this folder"); string(got) != string(down) {
		t.Fatalf("cursor on a numbered No: key %q, want Down", got)
	}
	if got := keyFor(onYes); string(got) != string(enter) {
		t.Fatalf("cursor on Yes: key %q, want Enter", got)
	}
	if got := keyFor("Loading development channels\n ❯ 1. I am using this for local development"); string(got) != string(enter) {
		t.Fatalf("dev channels: key %q, want Enter", got)
	}
	if keyFor("plain output") != nil {
		t.Fatal("a key for a screen with no prompt")
	}
}

func TestWatch_MovesOffNoBeforeConfirming(t *testing.T) {
	io := &fakeIO{captures: []string{
		"Is this a project you trust?\n ❯ No, exit\n   Yes, I trust this folder",
		"Is this a project you trust?\n   No, exit\n ❯ Yes, I trust this folder",
	}}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	go Watch(ctx, io, "pane-1")
	for {
		io.mu.Lock()
		sent := append([][]byte(nil), io.sent...)
		io.mu.Unlock()
		if len(sent) >= 2 {
			if string(sent[0]) != string(down) || string(sent[1]) != string(enter) {
				t.Fatalf("sent %q, want Down then Enter", sent)
			}
			return
		}
		if ctx.Err() != nil {
			t.Fatalf("sent only %q", sent)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// Past maxDowns the watch stops pressing Down but keeps watching, so a
// slow redraw that finally lands on Yes is still confirmed.
func TestPresses_DownsAreBoundedNotFinal(t *testing.T) {
	var p presses
	for i := 0; i < maxDowns; i++ {
		p.count(p.allow(down))
	}
	if p.allow(down) != nil {
		t.Fatal("a Down past the bound was allowed")
	}
	if string(p.allow(enter)) != string(enter) {
		t.Fatal("an Enter after the bound was dropped")
	}
}
