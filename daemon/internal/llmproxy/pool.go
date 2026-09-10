// Accounts form a failover pool: when the account answering a pane hits its
// limit the proxy marks it until the reset the response named and re-sends
// the same request on the next account in the pane's order — the pane never
// sees the 429. An upstream that does not answer at all fails over the same
// way, and so does a 401 from an account holding its OWN key; a 401 from a
// keyless pass-through does not, because that one is the PANE's login being
// rejected. A 5xx deliberately does not either, because a broken request
// would otherwise be walked through every account in turn.
// WHICH accounts are in that order, and in what sequence, is
// decided in candidates.go; this file is the health tracking and the resend.
// Claude subscription accounts are the case it was built for (each holds a
// long-lived setup-token the proxy injects per request, so which
// subscription answers is a routing decision rather than a pane login), and
// the mechanism is not specific to them. Every response also updates the
// account's health (usage percentages, reset times, last seen), which is what
// the settings Accounts tab reads.
package llmproxy

import (
	"bytes"
	"fmt"
	"io"
	"log"
	"math"
	"net/http"
	"net/http/httptrace"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// acctHealth is what the proxy has learned about one account from the
// responses that flowed through it. In-memory only: it repopulates from
// traffic after a restart, and one request re-discovers a still-standing
// limit.
type acctHealth struct {
	limitedUntil time.Time
	// downUntil sidelines an account whose upstream did not answer at all.
	// Separate from limitedUntil because the two are different facts with
	// different lifetimes: a quota limit carries a reset the upstream named
	// and can last days, an unreachable host is a guess that expires in
	// seconds. Folding them together would show "out of quota until Sunday"
	// for a refused connection.
	downUntil    time.Time
	lastError    string
	unauthorized bool
	lastSeen     time.Time
	lastStatus   int
	// Stored parsed and normalized — pct 0–100, resets as RFC3339; see
	// captureUtilization.
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
	now := h.now()
	return !a.unauthorized && now.After(a.limitedUntil) && now.After(a.downUntil)
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
func (h *healthState) observe(acct Account, resp *http.Response) {
	h.mu.Lock()
	defer h.mu.Unlock()
	a := h.get(acct.Name)
	a.lastSeen = h.now()
	a.lastStatus = resp.StatusCode
	// An upstream that answered is reachable, whatever it answered. That is a
	// different fact from whether it will SERVE us, which the switch below
	// decides, so a 404 clears the mark just as a 200 does.
	a.downUntil = time.Time{}
	a.lastError = ""
	captureUtilization(a, resp.Header)
	switch {
	// Quota first, for the reason failoverResponse gives: a limit can arrive
	// carrying 401, and testing the credential first recorded that as neither
	// a limit nor a rejection — no mark at all, so an exhausted account stayed
	// at the head of the order reading "active".
	case limitResponse(resp):
		a.limitedUntil = limitResetTime(resp.Header, h.now())
	case resp.StatusCode == http.StatusUnauthorized:
		// Only a rejection of the ACCOUNT's own credential belongs to the
		// account. Marking a pass-through here sidelined a healthy one:
		// usable() went false with no expiry, order() sank it, and the very
		// next request was answered and billed by the keyed account behind it
		// — the outcome failoverResponse refuses to cause on the first one.
		a.unauthorized = !forwardsPaneLogin(acct)
	case resp.StatusCode < 400:
		// The upstream accepted this account: whatever we believed is stale.
		a.unauthorized = false
		a.limitedUntil = time.Time{}
	}
}

// unreachableCooldown is how long a transport failure sidelines an account.
// Long enough that a burst of panes does not each rediscover the same dead
// upstream, short enough that a blip does not sideline a working primary for
// the rest of a session.
const unreachableCooldown = 30 * time.Second

// observeError folds a transport failure into the account's health. It is
// what observe cannot do: a refused connection or a dial timeout never
// reached an upstream, so there is no status and no headers to read, and
// until this existed such an account was never marked at all — every
// subsequent request picked the same dead upstream first.
func (h *healthState) observeError(name string, err error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	a := h.get(name)
	a.lastSeen = h.now()
	// The attempt produced no status. Leaving the previous one in place would
	// show a stale 200 next to a failure.
	a.lastStatus = 0
	a.downUntil = h.now().Add(unreachableCooldown)
	a.lastError = err.Error()
}

// failoverResponse reports whether another account should be given this
// request. Quota is the case the pool was built for.
//
// A 401 joins it for every account EXCEPT one whose upstream authenticates
// the credential the pane sent. There the rejection is the user's own login
// dying, and replaying it onto a keyed subscription hides that they must log
// in again AND bills a different account for the answer, so it surfaces.
// Everywhere else the rejection belongs to the account and the pane's user
// cannot fix it from the pane, so the next account gets the request.
//
// Deliberately NOT 5xx. A 500 can just as easily mean the request itself is
// broken, and retrying that walks one bad request through every account in
// the pool, spending each of them to collect the same error.
func failoverResponse(resp *http.Response, acct Account) bool {
	// Quota first. limitResponse accepts a "rejected" unified status on ANY
	// 4xx because the wire format is undocumented, so a limit can arrive
	// carrying 401 — and a keyless pass-through, which forwards the pane's own
	// subscription token to Anthropic, is exactly the account that receives
	// those headers. Testing the 401 first returned false for it and surfaced
	// a quota rejection as a dead credential.
	if limitResponse(resp) {
		return true
	}
	return resp.StatusCode == http.StatusUnauthorized && !forwardsPaneLogin(acct)
}

// forwardsPaneLogin reports whether a 401 from this account is the PANE's
// credential being rejected rather than the account's own.
//
// An empty APIKey IS that fact, and it is enforced rather than assumed:
// applyAuth attaches nothing to a keyless account, so restoreClientAuth
// leaves the pane's own credential on the wire, and hostPinViolation
// (llmproxy.go, "each pane's own login token would be forwarded to it")
// refuses to save a keyless account pointing anywhere except
// api.anthropic.com, chatgpt.com, localhost, or a private IP. There is no
// keyless account at a third-party upstream to distinguish, because settings
// will not store one.
//
// The ForwardsPaneLogin flag carries the same fact for the SYNTHETIC
// pass-through, which is fabricated in candidates.go and never passes through
// validation at all.
func forwardsPaneLogin(a Account) bool {
	return a.APIKey == "" || a.ForwardsPaneLogin
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
	return floorReset(parseReset(hdr, now), now)
}

// floorReset keeps a reset from landing in the past. usable() and statusRow
// both compare limitedUntil against now, so a stale reset — clock skew, a
// Retry-After of 0, a unix second already gone — is a mark that records
// nothing at all: the exhausted account stays at the head of the order and
// reads "active" while every request through it is rejected.
func floorReset(at, now time.Time) time.Time {
	if !at.After(now) {
		return now.Add(unreachableCooldown)
	}
	return at
}

func parseReset(hdr http.Header, now time.Time) time.Time {
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

// Header formats verified against live subscription traffic (2026-08-25):
// utilization is a fraction ("0.13" = 13%), resets are unix seconds
// ("1787652000"), status is "allowed"/"rejected…".
func captureUtilization(a *acctHealth, hdr http.Header) {
	if v := hdr.Get("anthropic-ratelimit-unified-5h-utilization"); v != "" {
		a.sessionPct = parsePct(v)
	}
	if v := hdr.Get("anthropic-ratelimit-unified-7d-utilization"); v != "" {
		a.weeklyPct = parsePct(v)
	}
	if v := hdr.Get("anthropic-ratelimit-unified-5h-reset"); v != "" {
		a.sessionReset = resetToRFC3339(v)
	}
	if v := hdr.Get("anthropic-ratelimit-unified-7d-reset"); v != "" {
		a.weeklyReset = resetToRFC3339(v)
	}
}

// parsePct turns a fraction utilization ("0.13") into 0–100 with one
// decimal. -1 = unparseable.
func parsePct(v string) float64 {
	f, err := strconv.ParseFloat(strings.TrimSpace(v), 64)
	if err != nil {
		return -1
	}
	return math.Round(f*1000) / 10
}

// resetToRFC3339 normalizes a reset header for display: unix seconds become
// RFC3339, anything else passes through as-is.
func resetToRFC3339(v string) string {
	if secs, err := strconv.ParseInt(v, 10, 64); err == nil {
		return time.Unix(secs, 0).UTC().Format(time.RFC3339)
	}
	return v
}

// poolTransport is the proxy's outbound transport: it sends the request, and
// when an account answers with a limit it marks the account and replays the
// request on the next one, rebuilt for THAT account. Replay needs the body
// the handler buffered; past the buffer cap the request is single-shot.
type poolTransport struct{ s *Service }

func (t poolTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	info, _ := req.Context().Value(infoKey{}).(*reqInfo)
	if info == nil {
		return http.DefaultTransport.RoundTrip(req)
	}
	// candidatesFor already holds a pool to one dialect, so this is the belt
	// to that braces: an account is never handed a path its upstream does
	// not serve, because doing so forwards a credential to it. Dropped here
	// rather than skipped mid-loop so the last member's own answer is still
	// what the client gets.
	pool := t.s.servableOnly(info.pool, info.rest, info.pane)
	attempt := req
	for i, acct := range pool {
		// One assignment for the whole loop: info.account means "the account
		// currently being attempted", and upstreamError and ModifyResponse
		// both read it after RoundTrip returns. Setting it on each return
		// path instead left the error path free to forget.
		info.account = acct
		if i > 0 {
			attempt = replayRequest(req, acct, info)
		}
		sent, wrote := traceDelivery(attempt)
		started := time.Now()
		resp, err := http.DefaultTransport.RoundTrip(sent)
		// Both exhaustion conditions in one place: a request whose body was
		// too large to buffer cannot be replayed at all, so it is "last" on
		// the first account no matter how long the pool is.
		last := !info.retryable || i == len(pool)-1
		if err != nil {
			if t.abandonAttempt(info, acct, sent, err, last, wrote.Load(), started) {
				return resp, err
			}
			log.Printf("llm: pane %s account %s is not answering (%v), retrying on %s", info.pane, acct.Name, err, pool[i+1].Name)
			continue
		}
		t.s.health.observe(acct, resp)
		if last || !failoverResponse(resp, acct) {
			return resp, nil
		}
		resp.Body.Close()
		log.Printf("llm: pane %s account %s answered %d, retrying on %s", info.pane, acct.Name, resp.StatusCode, pool[i+1].Name)
	}
	// Unreachable: the loop always returns on the last pool entry. An error
	// beats a fallthrough that would send a second live request.
	return nil, fmt.Errorf("llm pool for pane %s resolved empty", info.pane)
}

// abandonAttempt records what a transport failure says about the account and
// reports whether to give up rather than hand the request to the next one.
//
// A failure is ALWAYS recorded against the account. Whether the request may
// have been billed decides the replay; it does not decide whether the failure
// is worth showing. Fusing the two left an account whose connections break
// reading "active" in both lenses while every request through it 502'd.
//
// The one exception is a cancelled context, which is the PANE hanging up
// rather than the account failing: replayRequest clones that same context, so
// a mark here would sideline a wholly healthy pool on one Esc.
func (t poolTransport) abandonAttempt(info *reqInfo, acct Account, sent *http.Request, err error, last, delivered bool, started time.Time) bool {
	if ctxErr := sent.Context().Err(); ctxErr != nil {
		// Logged because nothing else records it: upstreamError returns early
		// on "context canceled" without writing a line. A pane that hung up
		// after 2s and an upstream that held the request for 90s arrive here
		// as the same error and are not the same problem, so the elapsed time
		// is the part worth keeping.
		log.Printf("llm: pane %s account %s abandoned after %s (%v)",
			info.pane, acct.Name, time.Since(started).Round(time.Second), ctxErr)
		return true
	}
	t.s.health.observeError(acct.Name, err)
	return delivered || last
}

// traceDelivery returns the request to send and a flag reporting whether the
// transport began writing it onto a connection. That is deliberately weaker
// than "reached the wire": WroteHeaders fires when the headers go into the
// connection's bufio.Writer, BEFORE the flush (request.go:733, flush just
// below it), so the flag is conservative in the safe direction — it can say
// delivered for a request that never left the buffer, never the reverse.
//
// Conservative is what this needs to be, because it gates the replay: a
// completion that was generated was billed, so replaying a request that DID
// arrive charges two subscriptions for one answer. Go's transport does not
// auto-retry a POST carrying a body unless GetBody is set, which bufferForRetry
// now does, so the buffered-but-unflushed case is retried by the transport
// itself before it can reach us.
//
// This asks the transport rather than reading the error, because the error
// cannot answer it. Matching "*net.OpError with Op == dial" looked like the
// same test and was not: a TLS handshake writes nothing either, yet fails as
// tls.RecordHeaderError ("server gave HTTP response to HTTPS client"), an
// x509 verification error, or a bare "TLS handshake timeout" string — none of
// them a net.OpError. Every production upstream is https, so one mistyped
// scheme or one intercepting proxy would have pinned a permanently failing
// account at the head of the order, with neither failover nor a mark.
//
// Atomic because the callback runs on the transport's write goroutine, which
// is not ordered against RoundTrip returning.
func traceDelivery(req *http.Request) (*http.Request, *atomic.Bool) {
	wrote := new(atomic.Bool)
	trace := &httptrace.ClientTrace{WroteHeaders: func() { wrote.Store(true) }}
	return req.WithContext(httptrace.WithClientTrace(req.Context(), trace)), wrote
}

// replayRequest re-aims the original outbound request at another account:
// same method and path, that account's upstream and credential, the buffered
// body.
func replayRequest(req *http.Request, acct Account, info *reqInfo) *http.Request {
	target, err := url.Parse(acct.BaseURL)
	if err != nil {
		// Unreachable past validation. Refusing the re-aim keeps the PREVIOUS
		// account's URL and auth — a consistent failure — instead of silently
		// carrying acct's credential to a host it was never configured for.
		log.Printf("llm: pane %s: replay on %s skipped — bad baseURL: %v", info.pane, acct.Name, err)
		return req
	}
	out := req.Clone(req.Context())
	u := *out.URL
	u.Scheme, u.Host = target.Scheme, target.Host
	u.Path = strings.TrimRight(target.Path, "/") + "/" + info.rest
	out.URL = &u
	out.Host = target.Host
	if info.body != nil {
		restoreBody(out, info.body)
		// info.body is what the PANE sent, so the rewrite has to run again
		// for this account: its model aliases and its system-turn verdict
		// are its own. Without this the replay carries the head account's
		// transforms to a different upstream.
		rewriteRequest(out, acct)
	}
	restoreClientAuth(out, info.clientAuth)
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
		r.GetBody = nil // a partly-read stream cannot be rewound
		r.Body = prefixedBody{io.MultiReader(bytes.NewReader(body), r.Body), r.Body}
		return
	}
	r.Body.Close()
	// restoreBody rather than a bare assignment: it sets GetBody with the body,
	// which is what lets net/http replay a request it never flushed (a
	// keep-alive the upstream had already closed) instead of handing it to us
	// looking like one that may have been served. rewriteRequest runs after
	// this and calls restoreBody again, so the two stay in step.
	restoreBody(r, body)
	info.body = body
	info.retryable = true
}
