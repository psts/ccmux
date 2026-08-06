package api

import (
	"context"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"ccmux.dev/ccmuxd/internal/manager"
	"ccmux.dev/ccmuxd/internal/session"
	"ccmux.dev/ccmuxd/internal/store"
	"ccmux.dev/ccmuxd/internal/tmux"
)

func clipboardTestServer(t *testing.T) *Server {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "reg.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return NewServer(manager.New(context.Background(), &tmux.Server{Socket: "unused"}, st))
}

// TestClipboard_Gates pins the request contract: loopback-only (the pipe runs
// on this host), pane header required, body required, unknown pane → 404.
func TestClipboard_Gates(t *testing.T) {
	s := clipboardTestServer(t)

	cases := []struct {
		name       string
		remoteAddr string
		pane       string
		body       string
		want       int
	}{
		{"non-loopback is refused", "100.64.0.9:4242", "%1", "hello", 403},
		{"missing pane header", "127.0.0.1:4242", "", "hello", 400},
		{"empty body", "127.0.0.1:4242", "%1", "", 400},
		{"unknown pane", "127.0.0.1:4242", "%1", "hello", 404},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest("POST", "/v1/clipboard", strings.NewReader(tc.body))
			req.RemoteAddr = tc.remoteAddr
			if tc.pane != "" {
				req.Header.Set("X-Ccmux-Pane", tc.pane)
			}
			rec := httptest.NewRecorder()
			s.clipboard(rec, req)
			if rec.Code != tc.want {
				t.Errorf("status = %d, want %d (%s)", rec.Code, tc.want, rec.Body)
			}
		})
	}
}

// TestFrameFor_Clipboard pins that a clipboard event maps to its OWN frame
// kind — the default arm would write the copied text into the terminal as
// pane output.
func TestFrameFor_Clipboard(t *testing.T) {
	msg := frameFor(session.Event{Kind: "clipboard", PaneID: "%3", Data: []byte("copied")})
	if msg.T != "clipboard" {
		t.Fatalf("frame kind = %q, want clipboard", msg.T)
	}
	if msg.Pane != "%3" || msg.Data == "" {
		t.Errorf("frame = %+v, want pane %%3 with data", msg)
	}
}
