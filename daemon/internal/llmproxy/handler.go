package llmproxy

import (
	"context"
	"log"
	"net/http"
	"net/http/httputil"
	"net/url"
	"slices"
	"strings"
)

// reqInfo travels with one proxied request. account starts as the chosen
// upstream and is updated by poolTransport to whoever actually answered;
// pool holds the failover candidates in try-order; body/retryable are what
// makes a replay possible (see bufferForRetry).
type reqInfo struct {
	account   Account
	pool      []Account
	pane      string
	rest      string
	body      []byte
	retryable bool
	// clientAuth is the credential the PANE sent, captured before any
	// account's auth replaced it. A replay has to start from this rather
	// than from whatever the previous account left in the headers: a keyless
	// account is a pass-through that forwards the pane's own login, and
	// applyAuth returns early for it, so without this a replay onto a
	// keyless account would carry the previous account's key to it.
	clientAuth http.Header
}

type infoKey struct{}

// Handler serves the pane-scoped LLM routes. It expects to be mounted on
// patterns of the form `/llm/pane/{pane}/{rest...}` (the api server owns the
// mux and the loopback guard); {rest} is the upstream path, e.g. v1/messages.
func (s *Service) Handler() http.Handler {
	proxy := &httputil.ReverseProxy{
		Rewrite:      rewrite,
		Transport:    poolTransport{s},
		ErrorHandler: upstreamError,
		// LLM responses stream as SSE; buffering a token stream would stall the
		// harness until the answer is complete.
		FlushInterval: -1,
		ModifyResponse: func(resp *http.Response) error {
			info := resp.Request.Context().Value(infoKey{}).(*reqInfo)
			log.Printf("llm: pane %s → %s %d %s /%s", info.pane, info.account.Name, resp.StatusCode, resp.Request.Method, info.rest)
			return nil
		},
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		pool, err := s.candidatesFor(r.PathValue("pane"))
		if err != nil {
			http.Error(w, "ccmux llm proxy: "+err.Error(), http.StatusBadGateway)
			return
		}
		account := pool[0]
		// A codex account is a pass-through to chatgpt.com, so ANY other
		// client misrouted onto it would hand its own bearer to OpenAI (a
		// hand-started claude in a pane whose codex route outlived the codex
		// process, say). Allowlist the codex provider's own surface instead
		// of denying known-foreign paths: refusing here fails the misroute
		// loudly on the first request instead of leaking a token per call.
		if !s.servablePath(r.PathValue("pane"), account, r.PathValue("rest")) {
			http.Error(w, s.dialectRefusal(r.PathValue("pane"), account), http.StatusBadGateway)
			return
		}
		info := &reqInfo{
			account: account, pool: pool,
			pane: r.PathValue("pane"), rest: r.PathValue("rest"),
			clientAuth: clientAuth(r.Header),
		}
		// Buffer BEFORE the rewrite, so info.body holds what the pane sent
		// rather than what this account's aliases and compat turned it into.
		// A replay re-runs the rewrite for whoever it lands on: two accounts
		// can alias models differently and disagree about system turns, and
		// replaying account[0]'s body onto account[1] would send the second
		// upstream a request built for the first.
		if len(pool) > 1 {
			bufferForRetry(r, info)
		}
		rewriteRequest(r, account)
		proxy.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), infoKey{}, info)))
	})
}

// dialectRefusal names the setting the reader has to change. The message
// used to say "clear the pane's llm route" unconditionally, which is wrong
// advice for the commonest way to hit this: a codex account chosen as the
// DEFAULT account, where the pane has no route to clear. Following it changed
// nothing while every shell pane kept failing, and the actual cause was never
// named.
func (s *Service) dialectRefusal(paneID string, a Account) string {
	routes, err := s.PaneRoutes()
	if err != nil {
		// Folding a read failure into "no pane route" would hand back a
		// confident instruction to change a setting that is not the cause —
		// the same defect this function exists to remove, one branch over.
		log.Printf("llm: pane %s: pane routes unreadable while refusing a request: %v", paneID, err)
		return "ccmux llm proxy: account " + a.Name + " (" + a.Kind +
			") does not serve this request; check the pane's llm route and the Default account under settings, Accounts"
	}
	if routes[paneID] != "" {
		return "ccmux llm proxy: this pane is routed to " + a.Kind + " account " + a.Name +
			", which does not serve this request — clear the pane's llm route"
	}
	return "ccmux llm proxy: the default account " + a.Name + " is a " + a.Kind +
		" account, which does not serve this request — pick a different Default account under settings, Accounts"
}

// firstSegment is the leading path element of a proxied request, and "" for
// any path carrying a dot segment.
//
// The dot-segment refusal is the load-bearing half. Go's ServeMux cleans the
// ESCAPED path, so "%2e%2e" survives routing and PathValue hands back a
// decoded "responses/../v1/messages"; rewrite clears RawPath and "." is not
// re-escaped, so the traversal went out on the wire for the upstream to
// normalize. Measured: POST /llm/pane/p1/responses/%2e%2e/%2e%2e/v1/messages
// reached chatgpt.com as /backend-api/codex/responses/../../v1/messages
// carrying a Claude subscription bearer, past the very allowlist that exists
// to stop it. Matching a whole segment rather than a prefix closes the
// sibling case ("responsesXYZ") in the same move.
func firstSegment(rest string) (string, bool) {
	for _, seg := range strings.Split(rest, "/") {
		if seg == "." || seg == ".." {
			return "", false
		}
	}
	head, _, _ := strings.Cut(rest, "/")
	// A leading slash ("%2fresponses" decodes to "/responses") gives an empty
	// head with no dot segment at all, so emptiness is refused on its own.
	return head, head != ""
}

// servablePath reports whether an account's upstream answers this path at
// all, in BOTH directions.
//
// One direction was missing and it leaked: nothing refused a codex-dialect
// path on a NON-codex account, so a codex pane that fell through to the
// keyless Anthropic pass-through (its harness's kinds matching nothing
// configured, or its codex kind unchecked) forwarded its ChatGPT OAuth
// bearer to api.anthropic.com untouched — applyAuth returns early for a
// keyless account, so the pane's own credential travelled. Verified reaching
// the upstream before this guard was made symmetric. v0.1.55 could not do it
// only because harness spawn pinned a codex pane route; removing that pin is
// what exposed it, so the guard has to hold the invariant instead.
// paneDialect is what the PANE is speaking, taken from its harness's own
// declaration rather than guessed from the path. "" when there is nothing to
// go on: no recorded harness, or one that declared no kinds.
func (s *Service) paneDialect(paneID string) string {
	kinds, _, known := s.harnessOf(paneID)
	if !known || len(kinds) == 0 {
		return ""
	}
	// AccountKinds is the set of account kinds a harness MAY talk to, not a
	// statement of what it speaks, and both editors let a user check codex
	// alongside the others. So a mixed declaration says nothing about the
	// dialect: reading "contains codex" as "speaks responses" refused every
	// request from such a pane on a keyless Anthropic account, including the
	// tier-3 pass-through, which the repo's own mixed-kind test treats as a
	// supported pairing. Only a sole declaration is evidence.
	if len(kinds) == 1 {
		return dialectOf(kinds[0])
	}
	if slices.Contains(kinds, "codex") {
		return "" // ambiguous: fall back to the path signal
	}
	return dialectMessages
}

// servablePath reports whether this account may serve this pane's request.
//
// The axis is the PANE's dialect against the ACCOUNT's, not the path shape
// and not whether the account holds a key. Both of those were tried and both
// were wrong in the same way. Path shape cannot say who is asking: /models is
// served by each side, and the OpenAI surface has more roots than a denylist
// enumerates (/chat/completions among them). And keyless cannot be the test,
// because validateKind REFUSES a key on a codex account — every codex account
// is keyless, so a keyless rule can never fire on that side, which is exactly
// how the /models leak survived a fix aimed at it.
//
// What actually matters: a keyless account is a pass-through that forwards
// the CALLER's own credential, so it must only ever be handed traffic of its
// own dialect. A keyed account replaces the credential outright and has
// nothing of the pane's to leak, so its upstream owns what it does not
// recognise.
func (s *Service) servablePath(paneID string, a Account, rest string) bool {
	if _, ok := firstSegment(rest); !ok {
		return false
	}
	if a.APIKey != "" {
		return true
	}
	// Keyless is a pass-through forwarding the CALLER's credential, so it may
	// only ever be handed its own dialect's surface — as an ALLOWLIST. A
	// denylist was written three times here and was incomplete every time:
	// first it missed the mirror direction, then its unparseable sentinel
	// allowed, then it named only /responses and /models and let
	// /chat/completions, /completions and /v1/responses through.
	if !servesSurface(a.Kind, rest) {
		return false
	}
	// And when the pane says what it speaks, the account has to match it.
	if want := s.paneDialect(paneID); want != "" && want != dialectOf(a.Kind) {
		return false
	}
	return true
}

// servesSurface reports whether a path is on the named kind's own surface.
//
// Measured rather than guessed: 30 days of this daemon's proxy log carried
// /v1/messages (40496), /api/hello (588, Claude Code's startup probe) and
// /v1/messages/count_tokens (216) on the Anthropic side, and /models (192)
// and /responses (75) on the codex side. Nothing else has ever been proxied.
// A v1-only allowlist would have broken /api/hello; a FIRST-segment one let
// /v1/responses through, which is why this matches two deep.
//
// An unmeasured path on a keyless account fails loudly and says which
// account refused it. That is the deliberate trade: this list only guards
// accounts that forward the CALLER's own credential, and a keyed account —
// which replaces it — is not restricted at all.
func servesSurface(kind, rest string) bool {
	one, two := twoSegments(rest)
	if kind == "codex" {
		return one == "models" || one == "responses"
	}
	return (one == "v1" && two == "messages") || (one == "api" && two == "hello")
}

func twoSegments(rest string) (string, string) {
	one, tail, _ := strings.Cut(rest, "/")
	two, _, _ := strings.Cut(tail, "/")
	return one, two
}

// servableOnly drops pool members that cannot serve this path, keeping the
// head (the handler has already checked it) so a pool never empties here.
func (s *Service) servableOnly(pool []Account, rest, pane string) []Account {
	out := make([]Account, 0, len(pool))
	for i, a := range pool {
		if i == 0 || s.servablePath(pane, a, rest) {
			out = append(out, a)
			continue
		}
		log.Printf("llm: pane %s: %s dropped from the failover order — it does not serve /%s", pane, a.Name, rest)
	}
	return out
}

// rewrite points the outbound request at the account's upstream: the pane
// prefix is dropped, the client's auth is replaced only when the account
// holds a credential (see Account).
func rewrite(pr *httputil.ProxyRequest) {
	info := pr.In.Context().Value(infoKey{}).(*reqInfo)
	target, err := url.Parse(info.account.BaseURL)
	if err != nil {
		// Unreachable past validation; SetURL(nil) would panic, an empty target
		// just fails the dial with a loggable error instead.
		target = &url.URL{}
	}
	pr.Out.URL.Path = "/" + info.rest
	pr.Out.URL.RawPath = ""
	pr.SetURL(target)
	pr.Out.Host = target.Host
	applyAuth(pr.Out, info.account)
}

// authHeaders is the credential surface clientAuth captures and
// restoreClientAuth puts back. ONE list, because the two must stay identical:
// a header added to the capture and not the restore leaks the previous
// account's credential on a replay, which is the leak the pair exists to
// close, and nothing would say so.
var authHeaders = []string{"Authorization", "x-api-key"}

// clientAuth snapshots the credential headers the pane sent, so a failover
// replay can be built from the pane's own request rather than from the last
// account's rewritten one.
func clientAuth(h http.Header) http.Header {
	out := http.Header{}
	for _, k := range authHeaders {
		if v := h.Get(k); v != "" {
			out.Set(k, v)
		}
	}
	return out
}

// restoreClientAuth puts the pane's own credential back on a replay before
// the next account's auth is applied over it. Without it a keyless account
// (whose applyAuth is a no-op by design) would be handed the previous
// account's key, and the pass-through would forward neither the pane's
// credential nor its own.
func restoreClientAuth(out *http.Request, saved http.Header) {
	for _, k := range authHeaders {
		if v := saved.Get(k); v != "" {
			out.Header.Set(k, v)
		} else {
			out.Header.Del(k)
		}
	}
}

// applyAuth swaps the client's credential for the account's. A keyless
// account is a pass-through — that is what carries a Claude Max OAuth bearer
// to Anthropic (or a codex pane's ChatGPT bearer to chatgpt.com) untouched.
// "openai" and "claude" credentials ride as a bearer, "anthropic" as an api
// key — a claude account's credential IS a subscription OAuth setup-token,
// never an api key.
func applyAuth(out *http.Request, a Account) {
	if a.APIKey == "" {
		return
	}
	// A meridian account's key is the token that STARTED its sidecar; the
	// sidecar authenticates upstream itself. Strip whatever the pane sent and
	// hand over a placeholder so nothing that looks like a credential crosses.
	if a.Kind == KindMeridian {
		out.Header.Del("Authorization")
		out.Header.Set("x-api-key", "ccmux")
		return
	}
	out.Header.Del("Authorization")
	out.Header.Del("x-api-key")
	if a.Kind == "openai" || a.Kind == "claude" {
		out.Header.Set("Authorization", "Bearer "+a.APIKey)
		return
	}
	out.Header.Set("x-api-key", a.APIKey)
}

func upstreamError(w http.ResponseWriter, r *http.Request, err error) {
	info, _ := r.Context().Value(infoKey{}).(*reqInfo)
	name := "unknown"
	if info != nil {
		name = info.account.Name
	}
	msg := "ccmux llm proxy: upstream " + name + " is not answering"
	if strings.Contains(err.Error(), "context canceled") {
		// The harness hung up first; nothing to report and nobody listening.
		return
	}
	pane := ""
	if info != nil {
		pane = info.pane
	}
	log.Printf("llm: pane %s → %s failed: %v", pane, name, err)
	http.Error(w, msg, http.StatusBadGateway)
}
