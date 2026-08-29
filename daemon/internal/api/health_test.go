package api

import (
	"encoding/json"
	"net/http/httptest"
	"testing"

	"ccmux.dev/ccmuxd/internal/version"
)

// TestHealth_Handshake pins the federation handshake shape: /v1/health must
// carry ok plus the build version and the wire-contract integer the hub gates on.
func TestHealth_Handshake(t *testing.T) {
	s := &Server{}
	rec := httptest.NewRecorder()
	s.health(rec, httptest.NewRequest("GET", "/v1/health", nil))

	if rec.Code != 200 {
		t.Fatalf("health = %d, want 200", rec.Code)
	}
	var got struct {
		OK       bool   `json:"ok"`
		Version  string `json:"version"`
		Contract int    `json:"contract"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v (%s)", err, rec.Body)
	}
	if !got.OK {
		t.Error("ok = false, want true")
	}
	if got.Contract != version.Contract {
		t.Errorf("contract = %d, want %d", got.Contract, version.Contract)
	}
	if got.Version == "" {
		t.Error("version is empty; want the build identity")
	}
}

// TestHealth_ChildrenFieldNames pins the three JSON names both lenses read.
// Neither lens can be compiled against this struct — daemon/web has no build
// step and Sources/ccmux has no toolchain on the daemon's CI host — so a
// renamed tag on childproc.Counts would silently switch both defunct-child
// warnings off forever with every Go test still green. This is the only place
// that contract is checked.
func TestHealth_ChildrenFieldNames(t *testing.T) {
	s := &Server{}
	rec := httptest.NewRecorder()
	s.health(rec, httptest.NewRequest("GET", "/v1/health", nil))

	var got map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v (%s)", err, rec.Body)
	}
	children, ok := got["children"].(map[string]any)
	if !ok {
		t.Fatalf(`"children" missing or not an object: %s`, rec.Body)
	}
	// daemon/web/app.js reads c.known and c.defunct; DaemonHealthService.swift
	// decodes all three. Changing any name here means changing both lenses.
	for _, field := range []string{"live", "defunct", "known"} {
		if _, present := children[field]; !present {
			t.Errorf(`children.%s missing — both lenses read it verbatim: %s`, field, rec.Body)
		}
	}
	if _, isBool := children["known"].(bool); !isBool {
		t.Errorf("children.known must stay a bool; lenses branch on it: %v", children["known"])
	}
}
