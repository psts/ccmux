package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// checkServer is llmServer with a claude account whose "Anthropic" is the
// given handler, plus a keyless account a check has nothing to ask with.
func checkServer(t *testing.T, upstream http.HandlerFunc) *Server {
	t.Helper()
	up := httptest.NewServer(upstream)
	t.Cleanup(up.Close)
	s := llmServer(t)
	rec := httptest.NewRecorder()
	s.putSettings(rec, httptest.NewRequest("PUT", "/v1/settings", strings.NewReader(`{
		"llmAccounts": [
			{"name": "sub", "kind": "claude", "baseURL": "`+up.URL+`", "apiKey": "sk-ant-oat01-secret"},
			{"name": "passthrough", "baseURL": "https://api.anthropic.com"}
		]}`)))
	if rec.Code != 200 {
		t.Fatalf("put = %d (%s)", rec.Code, rec.Body)
	}
	return s
}

func checkAccount(s *Server, name string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	r := httptest.NewRequest("POST", "/v1/llm/accounts/"+name+"/check", nil)
	r.SetPathValue("name", name)
	s.llmAccountCheck(rec, r)
	return rec
}

func TestLLMAccountCheck_AnswersWithTheFreshRow(t *testing.T) {
	s := checkServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("anthropic-ratelimit-unified-5h-utilization", "0.25")
		_, _ = w.Write([]byte(`{}`))
	})
	rec := checkAccount(s, "sub")
	if rec.Code != 200 {
		t.Fatalf("check = %d (%s)", rec.Code, rec.Body)
	}
	var row struct {
		Name, State string
		SessionPct  float64
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &row); err != nil {
		t.Fatal(err)
	}
	if row.Name != "sub" || row.State != "ok" || row.SessionPct != 25 {
		t.Fatalf("row = %+v; want sub, ok, 25%%", row)
	}
	if strings.Contains(rec.Body.String(), "sk-ant-oat01-secret") {
		t.Fatal("the check's answer carries the stored token")
	}
}

func TestLLMAccountCheck_StatusCodes(t *testing.T) {
	s := checkServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":{"message":"not for you"}}`))
	})
	cases := map[string]int{"sub": http.StatusBadGateway, "passthrough": http.StatusBadRequest, "ghost": http.StatusNotFound}
	for name, want := range cases {
		if rec := checkAccount(s, name); rec.Code != want {
			t.Errorf("%s: check = %d (%s); want %d", name, rec.Code, rec.Body, want)
		}
	}
	if rec := checkAccount(s, "sub"); !strings.Contains(rec.Body.String(), "not for you") {
		t.Errorf("refusal body = %s; want the upstream's reason", rec.Body)
	}
}
