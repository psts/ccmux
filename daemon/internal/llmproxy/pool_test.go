package llmproxy

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
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
		w.Header().Set("anthropic-ratelimit-unified-reset", strconv.FormatInt(time.Now().Add(resetIn).Unix(), 10))
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
		// Live header shapes: fraction utilization, unix-seconds reset.
		w.Header().Set("anthropic-ratelimit-unified-5h-utilization", "0.58")
		w.Header().Set("anthropic-ratelimit-unified-7d-utilization", "0.12")
		w.Header().Set("anthropic-ratelimit-unified-5h-reset", "1787652000")
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
	if st.State != "ok" || st.SessionPct != 58 || st.WeeklyPct != 12 {
		t.Fatalf("status = %+v, want fraction utilization as percent", st)
	}
	if st.SessionReset != "2026-08-25T10:00:00Z" {
		t.Fatalf("sessionReset = %q, want unix seconds normalized to RFC3339", st.SessionReset)
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
	cases := map[string]float64{"0.58": 58, "0.134": 13.4, "1": 100, "0.0": 0, "junk": -1}
	for in, want := range cases {
		if got := parsePct(in); got != want {
			t.Errorf("parsePct(%q) = %v, want %v", in, got, want)
		}
	}
}

// UpstreamModels asks an account's upstream for its /v1/models catalog with
// the account's own credential attached.
func TestUpstreamModels(t *testing.T) {
	var gotAuth string
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/models" {
			w.WriteHeader(404)
			return
		}
		gotAuth = r.Header.Get("x-api-key")
		_, _ = w.Write([]byte(`{"data":[{"id":"qwen3-4b-32k"},{"id":"gemma3"}]}`))
	}))
	defer up.Close()
	s := New(fakeStore{})
	accs := []Account{{Name: "local", Kind: "anthropic", BaseURL: up.URL, APIKey: "k"}}
	_ = s.Apply(&accs, nil)

	models, err := s.UpstreamModels("local")
	if err != nil {
		t.Fatal(err)
	}
	if len(models) != 2 || models[0] != "gemma3" || models[1] != "qwen3-4b-32k" {
		t.Fatalf("models = %v, want sorted ids", models)
	}
	if gotAuth != "k" {
		t.Fatalf("auth = %q, want the account key applied", gotAuth)
	}
	if _, err := s.UpstreamModels("ghost"); err == nil {
		t.Fatal("unknown account must error")
	}
}

// limitResetTime accepts every shape a limit response can name — the live
// unix-seconds form first (that is what Anthropic sends), then RFC3339,
// Retry-After, and a conservative default.
func TestLimitResetTime(t *testing.T) {
	now := time.Date(2026, 8, 25, 10, 0, 0, 0, time.UTC)
	hdr := func(k, v string) http.Header { h := http.Header{}; h.Set(k, v); return h }
	cases := []struct {
		h    http.Header
		want time.Time
	}{
		{hdr("anthropic-ratelimit-unified-reset", "1787652000"), time.Unix(1787652000, 0)},
		{hdr("anthropic-ratelimit-unified-reset", "2026-08-25T12:00:00Z"), time.Date(2026, 8, 25, 12, 0, 0, 0, time.UTC)},
		{hdr("Retry-After", "60"), now.Add(60 * time.Second)},
		{http.Header{}, now.Add(5 * time.Minute)},
	}
	for i, c := range cases {
		if got := limitResetTime(c.h, now); !got.Equal(c.want) {
			t.Errorf("case %d = %v, want %v", i, got, c.want)
		}
	}
}

// The 401 lifecycle: a rejected credential fails the request over to the
// next account, demotes the one that rejected it, and a later success through
// it clears the flag.
//
// This USED to surface the 401 to the pane, on the reasoning that claude has
// to show the auth error. Reversed deliberately: with a second account
// configured, a token that expired overnight cost a visible failure in the
// pane for something the pane's user cannot fix from there, and the Accounts
// tab already says "credential rejected" against the account itself. With one
// account there is nothing to fail over to and the 401 still arrives.
func TestUnauthorizedAccountLifecycle(t *testing.T) {
	statusA := 401
	upA := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(statusA)
	}))
	defer upA.Close()
	statusB := 200
	upB := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(statusB)
	}))
	defer upB.Close()
	s := claudePoolService(t, upA.URL, upB.URL)
	p := mount(s)
	defer p.Close()

	// The rejection is hidden from the pane by the failover, and recorded
	// against the account that produced it.
	if resp := call(t, p.URL, "/llm/pane/p1/v1/messages", ""); resp.StatusCode != 200 {
		t.Fatalf("status = %d, want the 401 hidden by failover", resp.StatusCode)
	}
	sts, _ := s.Statuses()
	if sts[0].State != "unauthorized" {
		t.Fatalf("status a = %+v, want unauthorized", sts[0])
	}
	// Next request prefers the healthy account.
	if resp := call(t, p.URL, "/llm/pane/p1/v1/messages", ""); resp.StatusCode != 200 {
		t.Fatalf("status = %d, want served by the healthy account", resp.StatusCode)
	}
	// B hits its limit, A's token was fixed out of band: the last-resort
	// retry through A succeeds and clears the flag.
	statusB = 429
	statusA = 200
	if resp := call(t, p.URL, "/llm/pane/p1/v1/messages", ""); resp.StatusCode != 200 {
		t.Fatalf("status = %d, want the last-resort account to answer", resp.StatusCode)
	}
	sts, _ = s.Statuses()
	if sts[0].State != "ok" {
		t.Fatalf("status a after success = %+v, want ok again", sts[0])
	}
}

// deadUpstream is a URL nothing is listening on: a server started so the port
// is real, then closed so connecting to it is refused. This is the outage
// case, distinct from an upstream that answers with an error.
func deadUpstream(t *testing.T) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	url := srv.URL
	srv.Close()
	return url
}

// statusOf finds one account's row by name; index order is not the point of
// these tests.
func statusOf(t *testing.T, s *Service, name string) AccountStatus {
	t.Helper()
	sts, err := s.Statuses()
	if err != nil {
		t.Fatal(err)
	}
	for _, st := range sts {
		if st.Name == name {
			return st
		}
	}
	t.Fatalf("no status row for %q", name)
	return AccountStatus{}
}

// An upstream that does not answer at all fails over exactly like a limit,
// and the dead account is sidelined so later requests skip it.
func TestFailoverOnDeadUpstream(t *testing.T) {
	dead := deadUpstream(t)
	hitsB := 0
	upB := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hitsB++
		w.WriteHeader(200)
	}))
	defer upB.Close()
	s := claudePoolService(t, dead, upB.URL)
	p := mount(s)
	defer p.Close()

	if resp := call(t, p.URL, "/llm/pane/p1/v1/messages", ""); resp.StatusCode != 200 {
		t.Fatalf("status = %d, want the outage hidden by failover", resp.StatusCode)
	}
	st := statusOf(t, s, "max-a")
	if st.State != "unreachable" || st.LastError == "" {
		t.Fatalf("status a = %+v, want unreachable with the error text", st)
	}
	// Sidelined: the second request must not pay the dial timeout again.
	call(t, p.URL, "/llm/pane/p1/v1/messages", "")
	if hitsB != 2 {
		t.Fatalf("live upstream hits = %d, want both requests to go straight to it", hitsB)
	}
}

// Nobody to fail over to: the pane gets the error, and the account is STILL
// marked. Marking only on a successful failover would leave a single-account
// daemon re-dialing a dead host forever.
func TestDeadUpstreamMarkedWithNoFailover(t *testing.T) {
	s := New(fakeStore{})
	accs := []Account{{Name: "solo", Kind: "claude", BaseURL: deadUpstream(t), APIKey: "sk-ant-oat01-x"}}
	route := "solo"
	if err := s.Apply(&accs, &route); err != nil {
		t.Fatal(err)
	}
	p := mount(s)
	defer p.Close()

	if resp := call(t, p.URL, "/llm/pane/p1/v1/messages", ""); resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("status = %d, want the outage surfaced when no account can serve", resp.StatusCode)
	}
	if st := statusOf(t, s, "solo"); st.State != "unreachable" {
		t.Fatalf("status = %+v, want unreachable", st)
	}
}

// A rejected credential is the account's problem, not the request's, so the
// next account gets the same request rather than the pane getting the 401.
func TestFailoverOnUnauthorized(t *testing.T) {
	hitsA := 0
	upA := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hitsA++
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer upA.Close()
	var got seen
	upB := upstream(t, &got)
	defer upB.Close()
	s := claudePoolService(t, upA.URL, upB.URL)
	p := mount(s)
	defer p.Close()

	if resp := call(t, p.URL, "/llm/pane/p1/v1/messages", ""); resp.StatusCode != 200 {
		t.Fatalf("status = %d, want the 401 hidden by failover", resp.StatusCode)
	}
	if got.auth != "Bearer sk-ant-oat01-bbb" {
		t.Fatalf("auth on retry = %q, want account B's token", got.auth)
	}
	if st := statusOf(t, s, "max-a"); st.State != "unauthorized" {
		t.Fatalf("status a = %+v, want unauthorized", st)
	}
	if hitsA != 1 {
		t.Fatalf("rejecting upstream hits = %d, want 1 then skipped", hitsA)
	}
}

// A 5xx does NOT fail over. It can just as easily mean the request is broken,
// and retrying that spends every account in the pool to collect one error.
func TestNoFailoverOnServerError(t *testing.T) {
	upA := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer upA.Close()
	hitsB := 0
	upB := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hitsB++
		w.WriteHeader(200)
	}))
	defer upB.Close()
	s := claudePoolService(t, upA.URL, upB.URL)
	p := mount(s)
	defer p.Close()

	if resp := call(t, p.URL, "/llm/pane/p1/v1/messages", ""); resp.StatusCode != http.StatusInternalServerError {
		t.Fatalf("status = %d, want the 500 passed through", resp.StatusCode)
	}
	if hitsB != 0 {
		t.Fatalf("second account hits = %d, want the 500 not walked through the pool", hitsB)
	}
}

// The cooldown is a guess, so a later success has to overrule it — otherwise
// a recovered host stays sidelined for the rest of the window.
func TestUnreachableClearsOnSuccess(t *testing.T) {
	h := newHealthState()
	h.observeError("a", errors.New("connection refused"))
	if h.usable("a") {
		t.Fatal("account usable immediately after a transport failure")
	}
	h.observe("a", &http.Response{StatusCode: 200, Header: http.Header{}})
	if !h.usable("a") {
		t.Fatal("account still sidelined after a successful response")
	}
	if st := statusRow(Account{Name: "a"}, h.get("a"), h.now()); st.State != "ok" || st.LastError != "" {
		t.Fatalf("status = %+v, want ok with the stale error dropped", st)
	}
}

// The cooldown lapses on its own: nothing has to succeed for the account to
// be tried again, which is what keeps a marked-dead account recoverable when
// it is the only one configured.
func TestUnreachableCooldownExpires(t *testing.T) {
	h := newHealthState()
	now := time.Now()
	h.now = func() time.Time { return now }
	h.observeError("a", errors.New("i/o timeout"))
	now = now.Add(unreachableCooldown + time.Second)
	if !h.usable("a") {
		t.Fatal("account still sidelined after the cooldown lapsed")
	}
}

// A pane that hangs up mid-request is not an outage. The outbound context is
// the inbound one (handler.go), and replayRequest clones it, so treating a
// cancellation as an account failure marked the WHOLE pool unreachable in one
// pass and sidelined every healthy account for the cooldown.
func TestClientCancelIsNotAnOutage(t *testing.T) {
	started, release := make(chan struct{}), make(chan struct{})
	upA := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(started)
		<-release
		w.WriteHeader(200)
	}))
	defer upA.Close()
	hitsB := 0
	upB := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hitsB++
		w.WriteHeader(200)
	}))
	defer upB.Close()
	s := claudePoolService(t, upA.URL, upB.URL)
	p := mount(s)
	defer p.Close()

	ctx, cancel := context.WithCancel(context.Background())
	req, _ := http.NewRequestWithContext(ctx, "POST", p.URL+"/llm/pane/p1/v1/messages", strings.NewReader(`{}`))
	done := make(chan struct{})
	go func() {
		defer close(done)
		if resp, err := http.DefaultClient.Do(req); err == nil {
			resp.Body.Close()
		}
	}()
	<-started // the first account has the request and is holding it
	cancel()
	<-done
	close(release)
	// Close waits for the proxy's own handler goroutine, which is the sync
	// point: without it the assertions race the transport's error path.
	p.Close()

	if hitsB != 0 {
		t.Fatalf("second account hits = %d, want a hang-up not walked through the pool", hitsB)
	}
	for _, name := range []string{"max-a", "max-b"} {
		if st := statusOf(t, s, name); st.State == "unreachable" {
			t.Fatalf("status %s = %+v, want the pane's own cancellation not recorded as an outage", name, st)
		}
	}
}

// An upstream that answers at all is reachable, whatever it answers: a 404
// after an outage has to clear the mark, not wait out the cooldown.
func TestAnyResponseClearsUnreachable(t *testing.T) {
	h := newHealthState()
	h.observeError("a", errors.New("connection refused"))
	h.observe("a", &http.Response{StatusCode: 404, Header: http.Header{}})
	if !h.usable("a") {
		t.Fatal("account still sidelined after the upstream answered")
	}
	if st := statusRow(Account{Name: "a"}, h.get("a"), h.now()); st.State != "ok" || st.LastError != "" {
		t.Fatalf("status = %+v, want ok with the stale error dropped", st)
	}
}
