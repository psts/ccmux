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

func clipboardTestServer(t *testing.T, token string) *Server {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "reg.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	s := NewServer(manager.New(context.Background(), &tmux.Server{Socket: "unused"}, st))
	s.SetClipboardToken(token)
	return s
}

// TestClipboard_Gates pins the request contract: loopback-only (the pipe runs
// on this host), per-boot token required (another ACCOUNT on this host must
// not be able to write lens clipboards), pane header + body required, unknown
// pane → 404, unarmed endpoint → 503.
func TestClipboard_Gates(t *testing.T) {
	cases := []struct {
		name       string
		serverTok  string
		remoteAddr string
		reqTok     string
		pane       string
		body       string
		want       int
	}{
		{"non-loopback is refused", "tok", "100.64.0.9:4242", "tok", "%1", "hello", 403},
		{"missing token", "tok", "127.0.0.1:4242", "", "%1", "hello", 401},
		{"wrong token", "tok", "127.0.0.1:4242", "nope", "%1", "hello", 401},
		{"endpoint unarmed (mint failed)", "", "127.0.0.1:4242", "", "%1", "hello", 503},
		{"missing pane header", "tok", "127.0.0.1:4242", "tok", "", "hello", 400},
		{"empty body", "tok", "127.0.0.1:4242", "tok", "%1", "", 400},
		{"unknown pane", "tok", "127.0.0.1:4242", "tok", "%1", "hello", 404},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := clipboardTestServer(t, tc.serverTok)
			req := httptest.NewRequest("POST", "/v1/clipboard", strings.NewReader(tc.body))
			req.RemoteAddr = tc.remoteAddr
			if tc.reqTok != "" {
				req.Header.Set("X-Ccmux-Clip", tc.reqTok)
			}
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
