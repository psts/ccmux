package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// keyServer is llmServer with one keyed and one keyless account stored, and
// the given WhoIs backend.
func keyServer(t *testing.T, res whoisResolver) *Server {
	t.Helper()
	s := llmServer(t)
	s.identity = res
	rec := httptest.NewRecorder()
	s.putSettings(rec, httptest.NewRequest("PUT", "/v1/settings", strings.NewReader(`{
		"llmAccounts": [
			{"name": "sub", "kind": "claude", "baseURL": "https://api.anthropic.com", "apiKey": "sk-ant-oat01-secret"},
			{"name": "passthrough", "baseURL": "https://api.anthropic.com"}
		]}`)))
	if rec.Code != 200 {
		t.Fatalf("put = %d (%s)", rec.Code, rec.Body)
	}
	return s
}

func revealKey(s *Server, name, remoteAddr string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/v1/llm/accounts/"+name+"/key", nil)
	r.SetPathValue("name", name)
	r.RemoteAddr = remoteAddr
	s.llmAccountKey(rec, r)
	return rec
}

func TestLLMAccountKey_VerifiedCallerGetsTheKey(t *testing.T) {
	s := keyServer(t, fakeResolver{login: "carol@example.com", ok: true})
	rec := revealKey(s, "sub", "100.64.0.9:5000")
	if rec.Code != 200 {
		t.Fatalf("reveal = %d (%s)", rec.Code, rec.Body)
	}
	var body struct{ Name, APIKey string }
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.Name != "sub" || body.APIKey != "sk-ant-oat01-secret" {
		t.Fatalf("body = %+v; want the stored key", body)
	}
}

func TestLLMAccountKey_UnvouchedCallerIsRefused(t *testing.T) {
	// No WhoIs answer, no owner: the daemon knows nothing about the caller.
	s := keyServer(t, declinedWhois{})
	if rec := revealKey(s, "sub", "127.0.0.1:5000"); rec.Code != http.StatusForbidden {
		t.Fatalf("reveal = %d (%s); want 403", rec.Code, rec.Body)
	}
	// A tagged tailnet node WhoIs declines is no better than loopback here.
	if rec := revealKey(s, "sub", "100.64.0.2:5000"); rec.Code != http.StatusForbidden {
		t.Fatalf("tagged reveal = %d (%s); want 403", rec.Code, rec.Body)
	}
}

func TestLLMAccountKey_OwnerOverLoopbackGetsTheKey(t *testing.T) {
	s := keyServer(t, declinedWhois{})
	if err := s.mgr.SetOwner("owner@example.com"); err != nil {
		t.Fatal(err)
	}
	if rec := revealKey(s, "sub", "127.0.0.1:5000"); rec.Code != 200 {
		t.Fatalf("owner reveal = %d (%s); want 200", rec.Code, rec.Body)
	}
	// The owner tier is bounded to loopback; from the tailnet it does not apply.
	if rec := revealKey(s, "sub", "100.64.0.2:5000"); rec.Code != http.StatusForbidden {
		t.Fatalf("owner-from-tailnet reveal = %d (%s); want 403", rec.Code, rec.Body)
	}
}

func TestLLMAccountKey_AliasTierIsNotEnough(t *testing.T) {
	// An alias vouches for routing, but it is claimed by a self-declared
	// ?user= that anyone WhoIs declines could send.
	s := keyServer(t, declinedWhois{})
	if err := s.mgr.SetIdentityAliases(map[string]string{"carol": "carol@example.com"}); err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/v1/llm/accounts/sub/key?user=carol", nil)
	r.SetPathValue("name", "sub")
	r.RemoteAddr = "100.64.0.2:5000"
	s.llmAccountKey(rec, r)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("alias reveal = %d (%s); want 403", rec.Code, rec.Body)
	}
}

func TestLLMAccountKey_NotFoundCases(t *testing.T) {
	s := keyServer(t, fakeResolver{login: "carol@example.com", ok: true})
	if rec := revealKey(s, "nope", "100.64.0.9:5000"); rec.Code != http.StatusNotFound {
		t.Fatalf("unknown account = %d; want 404", rec.Code)
	}
	rec := revealKey(s, "passthrough", "100.64.0.9:5000")
	if rec.Code != http.StatusNotFound || !strings.Contains(rec.Body.String(), "no stored key") {
		t.Fatalf("keyless account = %d (%s); want 404 saying no stored key", rec.Code, rec.Body)
	}
}
