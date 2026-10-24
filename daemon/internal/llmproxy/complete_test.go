package llmproxy

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// Complete asks the small model with the account's own token and returns
// the answer's text; a refusal names the account and the status.
func TestComplete_AsksWithTheAccountAndReturnsTheText(t *testing.T) {
	var auth string
	var body map[string]any
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth = r.Header.Get("Authorization")
		json.NewDecoder(r.Body).Decode(&body)
		w.Write([]byte(`{"content":[{"type":"text","text":" API, database, auth \n"}]}`))
	}))
	defer up.Close()
	s := checkService(t, up.URL)
	got, err := s.Complete(context.Background(), "what does it own?", 60)
	if err != nil || got != "API, database, auth" {
		t.Fatalf("Complete = %q, %v", got, err)
	}
	if auth != "Bearer sk-ant-oat01-x" || body["model"] != probeModel || body["max_tokens"].(float64) != 60 {
		t.Fatalf("request: auth %q body %v", auth, body)
	}

	down := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		w.Write([]byte(`{"error":{"message":"not allowed"}}`))
	}))
	defer down.Close()
	if _, err := checkService(t, down.URL).Complete(context.Background(), "x", 10); err == nil ||
		!strings.Contains(err.Error(), "max") || !strings.Contains(err.Error(), "403") {
		t.Fatalf("a refusal must name the account and status: %v", err)
	}
	if _, err := New(fakeStore{}).Complete(context.Background(), "x", 10); err == nil {
		t.Fatal("no account must be an error, not an empty answer")
	}
}

// A refused first subscription moves on to the next one, and an account
// that is not a claude subscription with a token is never asked.
func TestComplete_MovesOnToTheNextAccount(t *testing.T) {
	asked := map[string]int{}
	serve := func(name string, status int, body string) *httptest.Server {
		return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			asked[name]++
			w.WriteHeader(status)
			w.Write([]byte(body))
		}))
	}
	limited := serve("limited", http.StatusTooManyRequests, `{"error":{"message":"rate limited"}}`)
	defer limited.Close()
	open := serve("open", http.StatusOK, `{"content":[{"type":"text","text":"billing jobs"}]}`)
	defer open.Close()
	other := serve("other", http.StatusOK, `{"content":[{"type":"text","text":"wrong"}]}`)
	defer other.Close()

	s := New(fakeStore{})
	accs := []Account{
		{Name: "router", Kind: "openai", BaseURL: other.URL, APIKey: "k"},
		{Name: "nokey", Kind: "claude", BaseURL: other.URL},
		{Name: "limited", Kind: "claude", BaseURL: limited.URL, APIKey: "t1"},
		{Name: "open", Kind: "claude", BaseURL: open.URL, APIKey: "t2"},
	}
	route := "limited"
	if err := s.Apply(&accs, &route); err != nil {
		t.Fatal(err)
	}
	got, err := s.Complete(context.Background(), "x", 10)
	if err != nil || got != "billing jobs" {
		t.Fatalf("Complete = %q, %v", got, err)
	}
	if asked["limited"] != 1 || asked["open"] != 1 || asked["other"] != 0 {
		t.Fatalf("asked %v: the refused one, then the next; never a non-claude or keyless account", asked)
	}
}
