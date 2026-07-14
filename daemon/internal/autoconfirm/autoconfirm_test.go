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
