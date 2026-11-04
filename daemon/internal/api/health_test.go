package api

import (
	"encoding/json"
	"net/http/httptest"
	"testing"

	"ccmux.dev/ccmuxd/internal/version"
	"ccmux.dev/ccmuxd/web"
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

// TestHealth_ChildrenFieldNames pins the JSON names of the children counts.
// The web lens cannot be compiled against this struct (daemon/web has no build
// step), so a renamed tag on childproc.Counts would silently switch its
// defunct-child warning off forever with every Go test still green. This is
// the only place that contract is checked.
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
	// daemon/web/app.js reads c.known and c.defunct; live is part of the same
	// public shape. Changing any name here means changing the lens with it.
	for _, field := range []string{"live", "defunct", "known"} {
		if _, present := children[field]; !present {
			t.Errorf(`children.%s missing — the health shape is read verbatim: %s`, field, rec.Body)
		}
	}
	if _, isBool := children["known"].(bool); !isBool {
		t.Errorf("children.known must stay a bool; lenses branch on it: %v", children["known"])
	}
}

// TestHealth_WebBuild pins the field web/update.js reads to learn which lens
// build the daemon serves. Renamed or dropped, every open page would think it
// is current forever and never move onto a deploy, with every test green.
func TestHealth_WebBuild(t *testing.T) {
	s := &Server{}
	rec := httptest.NewRecorder()
	s.health(rec, httptest.NewRequest("GET", "/v1/health", nil))

	var got map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v (%s)", err, rec.Body)
	}
	if got["web"] != web.Build || web.Build == "" {
		t.Fatalf(`"web" = %v, want the lens build %q`, got["web"], web.Build)
	}
}
