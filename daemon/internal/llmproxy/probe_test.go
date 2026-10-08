package llmproxy

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"
)

// checkService holds one claude account pointed at url.
func checkService(t *testing.T, url string) *Service {
	t.Helper()
	s := New(fakeStore{})
	accs := []Account{{Name: "max", Kind: "claude", BaseURL: url, APIKey: "sk-ant-oat01-x"}}
	route := "max"
	if err := s.Apply(&accs, &route); err != nil {
		t.Fatal(err)
	}
	return s
}

// markLimited records the 429 a live request would have left behind.
func markLimited(s *Service, until time.Time) {
	hdr := http.Header{}
	hdr.Set("anthropic-ratelimit-unified-status", "rejected")
	hdr.Set("anthropic-ratelimit-unified-reset", strconv.FormatInt(until.Unix(), 10))
	s.health.observe(Account{Name: "max", Kind: "claude", APIKey: "x"},
		&http.Response{StatusCode: http.StatusTooManyRequests, Header: hdr})
}

// servedUpstream answers like an open subscription and keeps the last request.
func servedUpstream(t *testing.T, got **http.Request, body *map[string]any) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		*got = r.Clone(context.Background())
		_ = json.NewDecoder(r.Body).Decode(body)
		w.Header().Set("anthropic-ratelimit-unified-status", "allowed")
		w.Header().Set("anthropic-ratelimit-unified-5h-utilization", "0.02")
		w.Header().Set("anthropic-ratelimit-unified-7d-utilization", "0.39")
		_, _ = w.Write([]byte(`{"type":"message"}`))
	}))
}

// The case the check exists for: a limit lifted early. The stale mark goes,
// the usage the answer carried replaces the old numbers, and routing sees it.
func TestCheckClearsALiftedLimit(t *testing.T) {
	var got *http.Request
	var body map[string]any
	up := servedUpstream(t, &got, &body)
	defer up.Close()
	s := checkService(t, up.URL)
	markLimited(s, time.Now().Add(time.Hour))

	st, err := s.CheckAccount(context.Background(), "max")
	if err != nil {
		t.Fatal(err)
	}
	if st.State != "ok" || st.LimitedUntil != "" || st.SessionPct != 2 || st.WeeklyPct != 39 {
		t.Fatalf("row = %+v; want ok with the fresh usage", st)
	}
	if !s.health.usable("max") {
		t.Fatal("routing still skips the account after a served check")
	}
	if got.URL.Path != "/v1/messages" || got.Header.Get("Authorization") != "Bearer sk-ant-oat01-x" {
		t.Fatalf("probe = %s auth %q; want the account's token on /v1/messages", got.URL.Path, got.Header.Get("Authorization"))
	}
	if got.Header.Get("anthropic-version") == "" || got.Header.Get("anthropic-beta") != "oauth-2025-04-20" {
		t.Fatalf("probe headers = %v; want version and the oauth beta flag", got.Header)
	}
	if body["model"] != probeModel || body["max_tokens"] != float64(1) {
		t.Fatalf("probe body = %v; want the one-token haiku request", body)
	}
}

// Still out: the answer refreshes the mark with the reset it names.
func TestCheckRefreshesAStandingLimit(t *testing.T) {
	hits := 0
	up := limitedUpstream(t, &hits, 2*time.Hour)
	defer up.Close()
	s := checkService(t, up.URL)
	markLimited(s, time.Now().Add(time.Minute))

	st, err := s.CheckAccount(context.Background(), "max")
	if err != nil {
		t.Fatal(err)
	}
	until, _ := time.Parse(time.RFC3339, st.LimitedUntil)
	if st.State != "limited" || time.Until(until) < 90*time.Minute {
		t.Fatalf("row = %+v; want limited until the new reset", st)
	}
}

// Every answer that is not about quota records nothing. Each of these
// recorded would sideline or mislabel the account: 401 has no expiry,
// 400/404 would read "answering errors", and a redirect is not followed.
func TestCheckRefusalRecordsNothing(t *testing.T) {
	for _, code := range []int{400, 401, 404, 500, 302} {
		up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if code == 302 {
				w.Header().Set("Location", "https://elsewhere.example/")
			}
			w.WriteHeader(code)
			_, _ = w.Write([]byte(`{"error":{"type":"x","message":"model: nope"}}`))
		}))
		s := checkService(t, up.URL)
		markLimited(s, time.Now().Add(time.Hour))
		before := statusOf(t, s, "max")

		_, err := s.CheckAccount(context.Background(), "max")
		up.Close()
		if err == nil || !strings.Contains(err.Error(), strconv.Itoa(code)) || !strings.Contains(err.Error(), "nothing recorded") {
			t.Fatalf("%d: err = %v; want a refusal naming the status", code, err)
		}
		if code != 302 && !strings.Contains(err.Error(), "model: nope") {
			t.Fatalf("%d: err = %v; want the upstream's own message", code, err)
		}
		if after := statusOf(t, s, "max"); after != before {
			t.Fatalf("%d: row changed %+v -> %+v; a refusal must record nothing", code, before, after)
		}
	}
}

// An upstream that does not answer records nothing either: one failed check
// must not bench an account the way a failed live request does.
func TestCheckTransportErrorRecordsNothing(t *testing.T) {
	s := checkService(t, deadUpstream(t))
	if _, err := s.CheckAccount(context.Background(), "max"); err == nil {
		t.Fatal("check of a dead upstream succeeded")
	}
	if st := statusOf(t, s, "max"); st.State != "untried" {
		t.Fatalf("row = %+v; want it untouched", st)
	}
}

func TestCheckOnlyClaudeAccountsWithAToken(t *testing.T) {
	s := New(fakeStore{})
	accs := []Account{
		{Name: "max", Kind: "claude", BaseURL: "https://api.anthropic.com", APIKey: "t"},
		{Name: "local", Kind: "anthropic", BaseURL: "http://localhost:11434"},
		{Name: "pass", BaseURL: "https://api.anthropic.com"},
	}
	if err := s.Apply(&accs, nil); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"local", "pass"} {
		if _, err := s.CheckAccount(context.Background(), name); !errors.Is(err, ErrNotCheckable) {
			t.Errorf("%s: err = %v; want ErrNotCheckable", name, err)
		}
	}
	if _, err := s.CheckAccount(context.Background(), "ghost"); !errors.Is(err, ErrUnknownAccount) {
		t.Errorf("ghost: err = %v; want ErrUnknownAccount", err)
	}
}
