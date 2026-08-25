package llmproxy

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// limitedUpstream answers every request with an Anthropic-shaped limit
// response and counts how often it was asked.
func limitedUpstream(t *testing.T, hits *int, resetIn time.Duration) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		*hits++
		w.Header().Set("anthropic-ratelimit-unified-status", "rejected")
		w.Header().Set("anthropic-ratelimit-unified-reset", time.Now().Add(resetIn).UTC().Format(time.RFC3339))
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"error":{"type":"rate_limit_error"}}`))
	}))
}

func claudePoolService(t *testing.T, urlA, urlB string) *Service {
	t.Helper()
	s := New(fakeStore{})
	accs := []Account{
		{Name: "max-a", Kind: "claude", BaseURL: urlA, APIKey: "sk-ant-oat01-aaa"},
		{Name: "max-b", Kind: "claude", BaseURL: urlB, APIKey: "sk-ant-oat01-bbb"},
	}
	route := "max-a"
	if msg := s.Reject(&accs, &route); msg != "" {
		t.Fatalf("reject: %s", msg)
	}
	if err := s.Apply(&accs, &route); err != nil {
		t.Fatal(err)
	}
	return s
}

// A claude account's setup-token replaces the pane's own login as a bearer.
func TestClaudeAccountInjectsBearer(t *testing.T) {
	var got seen
	up := upstream(t, &got)
	defer up.Close()
	s := New(fakeStore{})
	accs := []Account{{Name: "max", Kind: "claude", BaseURL: up.URL, APIKey: "sk-ant-oat01-x"}}
	route := "max"
	_ = s.Apply(&accs, &route)
	p := mount(s)
	defer p.Close()

	call(t, p.URL, "/llm/pane/p1/v1/messages", "the-panes-own-login")
	if got.auth != "Bearer sk-ant-oat01-x" || got.apiKey != "" {
		t.Fatalf("auth = %q/%q, want the account's token as bearer", got.auth, got.apiKey)
	}
}

// The pool in action: the routed account answers with a limit, the SAME
// request replays on the next claude account, and the pane sees only the
// success. The limited account is then skipped until its reset.
func TestClaudeFailoverRetriesNextAccount(t *testing.T) {
	hitsA := 0
	upA := limitedUpstream(t, &hitsA, time.Hour)
	defer upA.Close()
	var got seen
	bodyCh := make(chan string, 2)
	upB := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		bodyCh <- string(b)
		got = seen{path: r.URL.Path, auth: r.Header.Get("Authorization"), apiKey: r.Header.Get("x-api-key"), host: r.Host}
		w.WriteHeader(200)
	}))
	defer upB.Close()
	s := claudePoolService(t, upA.URL, upB.URL)
	p := mount(s)
	defer p.Close()

	req, _ := http.NewRequest("POST", p.URL+"/llm/pane/p1/v1/messages", strings.NewReader(`{"model":"claude"}`))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d, want the failover to hide the limit", resp.StatusCode)
	}
	if hitsA != 1 {
		t.Fatalf("limited upstream hits = %d, want 1", hitsA)
	}
	if got.auth != "Bearer sk-ant-oat01-bbb" {
		t.Fatalf("auth on retry = %q, want account B's token", got.auth)
	}
	if b := <-bodyCh; b != `{"model":"claude"}` {
		t.Fatalf("replayed body = %q, want the original bytes", b)
	}

	// Second request: A is known-limited and must be skipped outright.
	call(t, p.URL, "/llm/pane/p1/v1/messages", "")
	<-bodyCh
	if hitsA != 1 {
		t.Fatalf("limited upstream hits after second request = %d, want still 1", hitsA)
	}

	sts, err := s.Statuses()
	if err != nil {
		t.Fatal(err)
	}
	if sts[0].Name != "max-a" || sts[0].State != "limited" || sts[0].LimitedUntil == "" {
		t.Fatalf("status a = %+v, want limited with a reset", sts[0])
	}
	if sts[1].State != "ok" {
		t.Fatalf("status b = %+v, want ok", sts[1])
	}
}

// Every pool member limited: the pane gets the real limit response, nothing
// loops.
func TestClaudeFailoverAllLimited(t *testing.T) {
	hitsA, hitsB := 0, 0
	upA := limitedUpstream(t, &hitsA, time.Hour)
	defer upA.Close()
	upB := limitedUpstream(t, &hitsB, time.Hour)
	defer upB.Close()
	s := claudePoolService(t, upA.URL, upB.URL)
	p := mount(s)
	defer p.Close()

	resp := call(t, p.URL, "/llm/pane/p1/v1/messages", "")
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want the limit surfaced when no account can serve", resp.StatusCode)
	}
	if hitsA != 1 || hitsB != 1 {
		t.Fatalf("hits = %d/%d, want one try each", hitsA, hitsB)
	}
}

// Usage headers on any response populate the account's status row.
func TestStatusesCaptureUtilization(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("anthropic-ratelimit-unified-5h-utilization", "0.58")
		w.Header().Set("anthropic-ratelimit-unified-7d-utilization", "12")
		w.Header().Set("anthropic-ratelimit-unified-5h-reset", "2026-08-25T12:00:00Z")
		w.WriteHeader(200)
	}))
	defer up.Close()
	s := New(fakeStore{})
	accs := []Account{{Name: "max", Kind: "claude", BaseURL: up.URL, APIKey: "sk-ant-oat01-x"}}
	route := "max"
	_ = s.Apply(&accs, &route)
	p := mount(s)
	defer p.Close()

	call(t, p.URL, "/llm/pane/p1/v1/messages", "")
	sts, err := s.Statuses()
	if err != nil {
		t.Fatal(err)
	}
	st := sts[0]
	if st.State != "ok" || st.SessionPct != 58 || st.WeeklyPct != 12 || st.SessionReset != "2026-08-25T12:00:00Z" {
		t.Fatalf("status = %+v, want captured utilization (fraction and percent forms)", st)
	}
}

func TestClaudeKindValidation(t *testing.T) {
	s := New(fakeStore{})
	cases := []struct {
		acc  Account
		want string
	}{
		{Account{Name: "m", Kind: "claude", APIKey: "t"}, ""}, // empty baseURL defaults to Anthropic
		{Account{Name: "m", Kind: "claude"}, "setup-token"},
		{Account{Name: "m", Kind: "claude", APIKey: "t", BaseURL: "https://attacker.example"}, "subscription token"},
	}
	for i, c := range cases {
		accs := []Account{c.acc}
		msg := s.Reject(&accs, nil)
		if c.want == "" && msg != "" {
			t.Errorf("case %d rejected: %s", i, msg)
		}
		if c.want != "" && !strings.Contains(msg, c.want) {
			t.Errorf("case %d = %q, want %q", i, msg, c.want)
		}
	}
}

func TestParsePct(t *testing.T) {
	cases := map[string]float64{"0.58": 58, "1": 100, "12": 12, "87.5": 87.5, "58%": 58, "junk": -1}
	for in, want := range cases {
		if got := parsePct(in); got != want {
			t.Errorf("parsePct(%q) = %v, want %v", in, got, want)
		}
	}
}
