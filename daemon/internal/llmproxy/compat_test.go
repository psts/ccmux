package llmproxy

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// A routed non-Anthropic account gets Claude Code's mid-conversation system
// turns downgraded to user turns — strict chat templates refuse a system
// message that is not first. The default Anthropic pass-through keeps them.
func TestSystemTurnsDowngradedForNonAnthropicUpstream(t *testing.T) {
	var gotBody []byte
	var gotLen int64
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotBody, _ = io.ReadAll(r.Body)
		gotLen = r.ContentLength
	}))
	defer up.Close()
	s := New(fakeStore{})
	accs := []Account{{Name: "ollama", BaseURL: up.URL}}
	route := "ollama"
	_ = s.Apply(&accs, &route)
	p := mount(s)
	defer p.Close()

	body := `{"model":"m","messages":[{"role":"user","content":"hi"},{"role":"system","content":"reminder"}]}`
	req, _ := http.NewRequest("POST", p.URL+"/llm/pane/p1/v1/messages", strings.NewReader(body))
	if _, err := http.DefaultClient.Do(req); err != nil {
		t.Fatal(err)
	}
	var got struct {
		Messages []struct{ Role, Content string } `json:"messages"`
	}
	if err := json.Unmarshal(gotBody, &got); err != nil {
		t.Fatalf("upstream body unparseable: %v (%s)", err, gotBody)
	}
	roles := []string{got.Messages[0].Role, got.Messages[1].Role}
	if roles[0] != "user" || roles[1] != "user" {
		t.Fatalf("roles = %v, want system turn downgraded to user", roles)
	}
	if got.Messages[1].Content != "reminder" {
		t.Fatalf("content = %q, want preserved", got.Messages[1].Content)
	}
	if gotLen != int64(len(gotBody)) {
		t.Fatalf("ContentLength %d does not match body %d", gotLen, len(gotBody))
	}
}

func TestSystemTurnsKeptForAnthropic(t *testing.T) {
	if needsSystemTurnCompat(Account{BaseURL: "https://api.anthropic.com"}) {
		t.Fatal("real Anthropic must keep system turns")
	}
	if !needsSystemTurnCompat(Account{BaseURL: "http://localhost:11434"}) {
		t.Fatal("non-Anthropic upstream must downgrade system turns")
	}
}

// A body the rewrite can't parse is forwarded byte-for-byte — the upstream's
// parser owns malformed requests.
func TestCompatForwardsUnparseableBodyUntouched(t *testing.T) {
	var gotBody []byte
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotBody, _ = io.ReadAll(r.Body)
	}))
	defer up.Close()
	s := New(fakeStore{})
	accs := []Account{{Name: "ollama", BaseURL: up.URL}}
	route := "ollama"
	_ = s.Apply(&accs, &route)
	p := mount(s)
	defer p.Close()

	body := `{"messages": not json`
	req, _ := http.NewRequest("POST", p.URL+"/llm/pane/p1/v1/messages", strings.NewReader(body))
	if _, err := http.DefaultClient.Do(req); err != nil {
		t.Fatal(err)
	}
	if string(gotBody) != body {
		t.Fatalf("body = %q, want forwarded untouched", gotBody)
	}
}

// Aliases rewrite the requested model on the way through — exact and prefix
// forms — so background calls hardwiring haiku names reach a model the
// upstream actually serves. Unmatched names pass unchanged.
func TestModelAliases(t *testing.T) {
	var gotBody []byte
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotBody, _ = io.ReadAll(r.Body)
	}))
	defer up.Close()
	s := New(fakeStore{})
	accs := []Account{{Name: "ollama", BaseURL: up.URL, ModelAliases: []ModelAlias{
		{From: "claude-haiku-*", To: "qwen3-4b-32k"},
		{From: "exact-model", To: "other"},
	}}}
	route := "ollama"
	_ = s.Apply(&accs, &route)
	p := mount(s)
	defer p.Close()

	send := func(model string) string {
		body := `{"model":"` + model + `","messages":[{"role":"user","content":"hi"}]}`
		req, _ := http.NewRequest("POST", p.URL+"/llm/pane/p1/v1/messages", strings.NewReader(body))
		if _, err := http.DefaultClient.Do(req); err != nil {
			t.Fatal(err)
		}
		var got struct {
			Model string `json:"model"`
		}
		if err := json.Unmarshal(gotBody, &got); err != nil {
			t.Fatalf("upstream body: %v (%s)", err, gotBody)
		}
		return got.Model
	}
	if m := send("claude-haiku-4-5-20251001"); m != "qwen3-4b-32k" {
		t.Fatalf("prefix alias = %q", m)
	}
	if m := send("exact-model"); m != "other" {
		t.Fatalf("exact alias = %q", m)
	}
	if m := send("qwen27:latest"); m != "qwen27:latest" {
		t.Fatalf("unmatched model rewritten to %q", m)
	}
}
