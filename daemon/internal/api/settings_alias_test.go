package api

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"

	"ccmux.dev/ccmuxd/internal/manager"
)

// aliasSettings drives the two settings handlers the way a lens does, and reads
// back the only alias field GET exposes.
func aliasSettings(t *testing.T, s *Server) (get func() []string, put func(string) int) {
	t.Helper()
	get = func() []string {
		rec := httptest.NewRecorder()
		s.getSettings(rec, httptest.NewRequest("GET", "/v1/settings", nil))
		var got struct {
			Names []string `json:"identityAliasNames"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
			t.Fatalf("decode: %v", err)
		}
		return got.Names
	}
	put = func(body string) int {
		rec := httptest.NewRecorder()
		s.putSettings(rec, httptest.NewRequest("PUT", "/v1/settings", strings.NewReader(body)))
		return rec.Code
	}
	return get, put
}

func TestSettings_IdentityAliasRoundtrip(t *testing.T) {
	s := settingsServer(t)
	get, put := aliasSettings(t, s)

	if got := get(); len(got) != 0 {
		t.Fatalf("fresh registry reports aliases %v, want none", got)
	}

	if code := put(`{"identityAliases":{"Patric Sandelin":"p@example.com"}}`); code != 200 {
		t.Fatalf("put = %d", code)
	}
	if got := get(); len(got) != 1 || got[0] != "patric sandelin" {
		t.Errorf("names = %v, want the lowercased name", got)
	}
	// The value has to have actually landed where identity resolution reads it.
	if got := s.mgr.ResolveAlias("Patric Sandelin"); got != "p@example.com" {
		t.Errorf("ResolveAlias = %q, want the login from the PUT", got)
	}
}

// GET is unauthenticated and the values are verified logins — emails. It reports
// which names are aliased, never what they map to, matching the write-only rule
// the neighbouring dev-hostname secrets follow.
func TestSettings_IdentityAliasLoginsAreNotReadable(t *testing.T) {
	s := settingsServer(t)
	_, put := aliasSettings(t, s)
	if code := put(`{"identityAliases":{"someone":"secret@example.com"}}`); code != 200 {
		t.Fatalf("put = %d", code)
	}

	rec := httptest.NewRecorder()
	s.getSettings(rec, httptest.NewRequest("GET", "/v1/settings", nil))

	if strings.Contains(rec.Body.String(), "secret@example.com") {
		t.Errorf("GET /v1/settings leaked an aliased login:\n%s", rec.Body.String())
	}
}

// A PUT replaces the whole map. Merging would leave an alias you can't delete by
// sending the map without it.
func TestSettings_IdentityAliasPutReplacesRatherThanMerges(t *testing.T) {
	s := settingsServer(t)
	get, put := aliasSettings(t, s)

	put(`{"identityAliases":{"first":"a@example.com","second":"b@example.com"}}`)
	if got := get(); len(got) != 2 {
		t.Fatalf("names = %v, want both", got)
	}

	put(`{"identityAliases":{"second":"b@example.com"}}`)
	if got := get(); len(got) != 1 || got[0] != "second" {
		t.Errorf("names = %v, want only the one that was re-sent", got)
	}
	if got := s.mgr.ResolveAlias("first"); got != "first" {
		t.Errorf("dropped alias still resolves to %q", got)
	}
}

// An omitted field means "leave this alone". Without this, saving any unrelated
// setting from a UI would wipe the alias map.
func TestSettings_OmittedAliasFieldLeavesTheMapAlone(t *testing.T) {
	s := settingsServer(t)
	get, put := aliasSettings(t, s)
	put(`{"identityAliases":{"keep":"keep@example.com"}}`)

	if code := put(`{"owner":"keep-tester@example.com"}`); code != 200 {
		t.Fatalf("put = %d", code)
	}
	if got := get(); len(got) != 1 || got[0] != "keep" {
		t.Errorf("names = %v, want the alias untouched by an unrelated write", got)
	}
}

// A row missing either side is refused with a 400 and persists nothing, rather
// than being silently dropped behind a 200.
func TestSettings_IncompleteAliasIsRejected(t *testing.T) {
	s := settingsServer(t)
	get, put := aliasSettings(t, s)
	put(`{"identityAliases":{"good":"good@example.com"}}`)

	if code := put(`{"identityAliases":{"orphan":""}}`); code != 400 {
		t.Errorf("put = %d, want 400", code)
	}
	if got := get(); len(got) != 1 || got[0] != "good" {
		t.Errorf("names = %v, want the rejected write to have changed nothing", got)
	}
}

// resolveIdentity reads the alias map, and it is reached from /v1/attach and
// every /v1/push route. A store-less Manager is a construction several tests in
// this package already use, so it must answer rather than panic inside a handler.
func TestResolveIdentity_StorelessManagerDoesNotPanic(t *testing.T) {
	s := NewServer(manager.New(context.Background(), nil, nil))
	s.identity = fakeResolver{ok: false}

	id := s.resolveIdentity(req("127.0.0.1:5000", "?user=dave"))

	if id.Login != "dave" {
		t.Errorf("login = %q, want the declared name when no settings exist", id.Login)
	}
}
