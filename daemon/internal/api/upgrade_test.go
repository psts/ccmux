package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"ccmux.dev/ccmuxd/internal/version"
)

// post hits the handler directly — the route needs no manager state.
func postUpgrade(t *testing.T, s *Server, body string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/v1/upgrade", strings.NewReader(body))
	s.selfUpgrade(rec, req)
	return rec
}

func TestSelfUpgrade(t *testing.T) {
	oldBuild := version.Build
	t.Cleanup(func() { version.Build = oldBuild; upgrading.Store(false) })

	var spawned []string
	s := &Server{spawnUpgrade: func(tag string) error { spawned = append(spawned, tag); return nil }}

	// A source build refuses remote upgrades — it's a developer's working binary.
	version.Build = "0.1.13-dirty"
	if rec := postUpgrade(t, s, `{"version":"v0.1.15"}`); rec.Code != http.StatusConflict {
		t.Fatalf("source build: %d, want 409", rec.Code)
	}

	version.Build = "0.1.13"
	// Bad tags are rejected before anything spawns.
	if rec := postUpgrade(t, s, `{"version":"latest"}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("bad tag: %d, want 400", rec.Code)
	}
	// Same version is a friendly no-op.
	if rec := postUpgrade(t, s, `{"version":"v0.1.13"}`); rec.Code != http.StatusOK {
		t.Fatalf("same version: %d, want 200", rec.Code)
	}
	if len(spawned) != 0 {
		t.Fatalf("nothing should have spawned yet, got %v", spawned)
	}
	// The real trigger spawns exactly once; a second call while "upgrading"
	// is refused (the flag only clears via the restart).
	if rec := postUpgrade(t, s, `{"version":"v0.1.15"}`); rec.Code != http.StatusAccepted {
		t.Fatalf("upgrade: %d, want 202", rec.Code)
	}
	if rec := postUpgrade(t, s, `{"version":"v0.1.15"}`); rec.Code != http.StatusConflict {
		t.Fatalf("concurrent upgrade: %d, want 409", rec.Code)
	}
	if len(spawned) != 1 || spawned[0] != "v0.1.15" {
		t.Fatalf("spawned = %v, want [v0.1.15]", spawned)
	}
}
