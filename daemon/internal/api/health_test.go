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
