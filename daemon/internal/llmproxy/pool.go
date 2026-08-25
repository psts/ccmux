// Claude subscription accounts form a failover pool: each holds a long-lived
// setup-token (claude setup-token) the proxy injects per request, so WHICH
// subscription answers is a routing decision, not a pane login. When an
// account hits its limit the proxy marks it until the reset the response
// named and re-sends the same request on the next claude account — the pane
// never sees the 429. Every response also updates the account's health
// (usage percentages, reset times, last seen), which is what the settings
// Accounts tab reads.
package llmproxy

import (
	"bytes"
	"io"
	"log"
	"math"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

// acctHealth is what the proxy has learned about one account from the
// responses that flowed through it. In-memory only: it repopulates from
// traffic after a restart, and one request re-discovers a still-standing
// limit.
type acctHealth struct {
	limitedUntil time.Time
	unauthorized bool
	lastSeen     time.Time
	lastStatus   int
	// Utilization headers are stored raw alongside the parsed value — the
	// format (fraction vs percent) is undocumented, so the UI gets both.
	sessionPct   float64 // -1 = never seen
	weeklyPct    float64
	sessionReset string
	weeklyReset  string
}

type healthState struct {
	mu     sync.Mutex
	byName map[string]*acctHealth
	// now is time.Now in production; tests inject.
	now func() time.Time
}

func newHealthState() *healthState {
	return &healthState{byName: map[string]*acctHealth{}, now: time.Now}
}

func (h *healthState) get(name string) *acctHealth {
	a, ok := h.byName[name]
	if !ok {
		a = &acctHealth{sessionPct: -1, weeklyPct: -1}
		h.byName[name] = a
	}
	return a
}

// usable reports whether an account is worth trying right now.
func (h *healthState) usable(name string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	a := h.get(name)
	return !a.unauthorized && h.now().After(a.limitedUntil)
}

// order returns the pool with unusable accounts moved to the back — they are
// kept as a last resort (their limit may have lifted early; a real error
// beats refusing to try at all).
func (h *healthState) order(pool []Account) []Account {
	usable := make([]Account, 0, len(pool))
	rest := make([]Account, 0)
	for _, a := range pool {
		if h.usable(a.Name) {
			usable = append(usable, a)
		} else {
			rest = append(rest, a)
		}
	}
	return append(usable, rest...)
}

// observe folds one upstream response into the account's health.
func (h *healthState) observe(name string, resp *http.Response) {
	h.mu.Lock()
	defer h.mu.Unlock()
	a := h.get(name)
	a.lastSeen = h.now()
	a.lastStatus = resp.StatusCode
	captureUtilization(a, resp.Header)
	switch {
	case resp.StatusCode == http.StatusUnauthorized:
		a.unauthorized = true
	case limitResponse(resp):
		a.limitedUntil = limitResetTime(resp.Header, h.now())
	case resp.StatusCode < 400:
		// The upstream accepted this account: whatever we believed is stale.
		a.unauthorized = false
		a.limitedUntil = time.Time{}
	}
}

// limitResponse recognizes "this account is out of quota": a plain 429, or
// Anthropic's unified status saying rejected while the HTTP status is a 4xx.
func limitResponse(resp *http.Response) bool {
	if resp.StatusCode == http.StatusTooManyRequests {
		return true
	}
	st := resp.Header.Get("anthropic-ratelimit-unified-status")
	return strings.HasPrefix(st, "rejected") && resp.StatusCode >= 400
}

// limitResetTime picks when to try the account again: the unified reset
// header (unix seconds or RFC3339 — the format is undocumented, accept
// both), else Retry-After seconds, else a conservative five minutes.
func limitResetTime(hdr http.Header, now time.Time) time.Time {
	if v := hdr.Get("anthropic-ratelimit-unified-reset"); v != "" {
		if secs, err := strconv.ParseInt(v, 10, 64); err == nil {
			return time.Unix(secs, 0)
		}
		if t, err := time.Parse(time.RFC3339, v); err == nil {
			return t
		}
	}
	if v := hdr.Get("Retry-After"); v != "" {
		if secs, err := strconv.Atoi(v); err == nil {
			return now.Add(time.Duration(secs) * time.Second)
		}
	}
	return now.Add(5 * time.Minute)
}

func captureUtilization(a *acctHealth, hdr http.Header) {
	if v := hdr.Get("anthropic-ratelimit-unified-5h-utilization"); v != "" {
		a.sessionPct = parsePct(v)
	}
	if v := hdr.Get("anthropic-ratelimit-unified-7d-utilization"); v != "" {
		a.weeklyPct = parsePct(v)
	}
	if v := hdr.Get("anthropic-ratelimit-unified-5h-reset"); v != "" {
		a.sessionReset = v
	}
	if v := hdr.Get("anthropic-ratelimit-unified-7d-reset"); v != "" {
		a.weeklyReset = v
	}
}

// parsePct normalizes an undocumented utilization value to 0–100: a value at
// or under 1.5 is read as a fraction (0.58 → 58%), anything larger as a
// percent already. -1 = unparseable.
func parsePct(v string) float64 {
	f, err := strconv.ParseFloat(strings.TrimSuffix(strings.TrimSpace(v), "%"), 64)
	if err != nil {
		return -1
	}
	if f <= 1.5 {
		f *= 100
	}
	return math.Round(f*10) / 10
}

// resolvePool answers who should serve a pane's request, in order: the
// routed account first, then — only when it is a claude subscription — the
// other claude accounts as failover, healthiest first.
func (s *Service) resolvePool(paneID string) ([]Account, error) {
	preferred, err := s.resolve(paneID)
	if err != nil {
		return nil, err
	}
	if preferred.Kind != "claude" {
		return []Account{preferred}, nil
	}
	accs, err := s.Accounts()
	if err != nil {
		return nil, err
	}
	pool := []Account{preferred}
	for _, a := range accs {
		if a.Kind == "claude" && a.Name != preferred.Name {
			pool = append(pool, a)
		}
	}
	return s.health.order(pool), nil
}

// poolTransport is the proxy's outbound transport: it sends the request, and
// when a claude pool account answers with a limit it marks the account and
// replays the same request on the next one. Replay needs the body the
// handler buffered; past the buffer cap the request is single-shot.
type poolTransport struct{ s *Service }

func (t poolTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	info, _ := req.Context().Value(infoKey{}).(*reqInfo)
	if info == nil {
		return http.DefaultTransport.RoundTrip(req)
	}
	attempt := req
	for i, acct := range info.pool {
		if i > 0 {
			attempt = replayRequest(req, acct, info)
		}
		resp, err := http.DefaultTransport.RoundTrip(attempt)
		if err != nil {
			return resp, err
		}
		t.s.health.observe(acct.Name, resp)
		last := i == len(info.pool)-1
		if !info.retryable || last || !limitResponse(resp) {
			info.account = acct
			return resp, nil
		}
		resp.Body.Close()
		log.Printf("llm: pane %s account %s is at its limit, retrying on %s", info.pane, acct.Name, info.pool[i+1].Name)
	}
	// Unreachable: the loop always returns on the last pool entry.
	return http.DefaultTransport.RoundTrip(req)
}

// replayRequest re-aims the original outbound request at another account:
// same method and path, that account's upstream and credential, the buffered
// body.
func replayRequest(req *http.Request, acct Account, info *reqInfo) *http.Request {
	out := req.Clone(req.Context())
	if target, err := url.Parse(acct.BaseURL); err == nil {
		u := *out.URL
		u.Scheme, u.Host = target.Scheme, target.Host
		u.Path = strings.TrimRight(target.Path, "/") + "/" + info.rest
		out.URL = &u
		out.Host = target.Host
	}
	if info.body != nil {
		out.Body = io.NopCloser(bytes.NewReader(info.body))
		out.ContentLength = int64(len(info.body))
	}
	applyAuth(out, acct)
	return out
}

// bufferForRetry makes the request replayable when it can be done safely: a
// bodyless request always is; a body within the compat cap is buffered once.
// Anything larger stays single-shot — the original bytes stream through
// untouched and a limit response simply passes to the client.
func bufferForRetry(r *http.Request, info *reqInfo) {
	if r.Body == nil || r.Body == http.NoBody {
		info.retryable = true
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, maxCompatBody+1))
	if err != nil || len(body) > maxCompatBody {
		r.Body = prefixedBody{io.MultiReader(bytes.NewReader(body), r.Body), r.Body}
		return
	}
	r.Body.Close()
	r.Body = io.NopCloser(bytes.NewReader(body))
	r.ContentLength = int64(len(body))
	info.body = body
	info.retryable = true
}
