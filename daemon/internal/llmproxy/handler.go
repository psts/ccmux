package llmproxy

import (
	"context"
	"log"
	"net/http"
	"net/http/httputil"
	"net/url"
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
		pool, err := s.resolvePool(r.PathValue("pane"))
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
		if !servablePath(account, r.PathValue("rest")) {
			http.Error(w, "ccmux llm proxy: pane is routed to codex account "+account.Name+", which serves only codex traffic — clear the pane's llm route", http.StatusBadGateway)
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

// codexServablePath is the codex CLI provider's own surface — the only
// paths a codex account forwards (probed against codex-cli 0.149.1: /models
// and /responses, relative to the provider base URL).
func codexServablePath(rest string) bool {
	return rest == "models" || rest == "responses" ||
		strings.HasPrefix(rest, "models/") || strings.HasPrefix(rest, "responses/")
}

// servablePath reports whether an account's upstream answers this path at
// all. Only codex accounts restrict it; every other kind speaks the Anthropic
// Messages surface, where the upstream owns what it does not recognize.
func servablePath(a Account, rest string) bool {
	return a.Kind != "codex" || codexServablePath(rest)
}

// servableOnly drops pool members that cannot serve this path, keeping the
// head (the handler has already checked it) so a pool never empties here.
func servableOnly(pool []Account, rest, pane string) []Account {
	out := make([]Account, 0, len(pool))
	for i, a := range pool {
		if i == 0 || servablePath(a, rest) {
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

// clientAuth snapshots the credential headers the pane sent, so a failover
// replay can be built from the pane's own request rather than from the last
// account's rewritten one.
func clientAuth(h http.Header) http.Header {
	out := http.Header{}
	for _, k := range []string{"Authorization", "x-api-key"} {
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
	for _, k := range []string{"Authorization", "x-api-key"} {
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
