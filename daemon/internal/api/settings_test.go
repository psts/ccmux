package api

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"ccmux.dev/ccmuxd/internal/manager"
	"ccmux.dev/ccmuxd/internal/store"
	"ccmux.dev/ccmuxd/internal/tmux"
)

// settingsServer builds a Server around a real store-backed manager (no tmux
// started — the settings paths never touch it).
func settingsServer(t *testing.T) *Server {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "reg.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	mgr := manager.New(context.Background(), &tmux.Server{Socket: "unused"}, st)
	return &Server{mgr: mgr}
}

func TestSettings_StartupCommandRoundtrip(t *testing.T) {
	s := settingsServer(t)

	get := func() string {
		rec := httptest.NewRecorder()
		s.getSettings(rec, httptest.NewRequest("GET", "/v1/settings", nil))
		var got map[string]string
		if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
			t.Fatalf("decode: %v", err)
		}
		return got["startupCommand"]
	}
	put := func(body string) string {
		rec := httptest.NewRecorder()
		s.putSettings(rec, httptest.NewRequest("PUT", "/v1/settings", strings.NewReader(body)))
		if rec.Code != 200 {
			t.Fatalf("put = %d (%s)", rec.Code, rec.Body)
		}
		var got map[string]string
		_ = json.Unmarshal(rec.Body.Bytes(), &got)
		return got["startupCommand"]
	}

	// Unset → the peers-enabled fallback, resolved.
	if got := get(); got != manager.FallbackStartupCommand {
		t.Fatalf("default = %q, want fallback", got)
	}
	// Set a custom command → persisted and echoed.
	if got := put(`{"startupCommand":"claude --continue"}`); got != "claude --continue" {
		t.Fatalf("after put = %q", got)
	}
	if got := get(); got != "claude --continue" {
		t.Fatalf("get after put = %q", got)
	}
	// Empty (whitespace) resets to the fallback.
	if got := put(`{"startupCommand":"   "}`); got != manager.FallbackStartupCommand {
		t.Fatalf("after reset = %q, want fallback", got)
	}
	// PUT without the field changes nothing.
	put(`{"startupCommand":"claude --continue"}`)
	if got := put(`{}`); got != "claude --continue" {
		t.Fatalf("field-less put changed setting to %q", got)
	}
}
