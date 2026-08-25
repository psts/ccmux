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
		// A codex account is a pass-through to chatgpt.com, so an
		// Anthropic-dialect client misrouted onto it would hand its Claude
		// bearer to OpenAI. Refusing here fails the misroute loudly on the
		// first request instead of leaking a token per call.
		if account.Kind == "codex" && strings.Contains(r.URL.Path, "/v1/messages") {
			http.Error(w, "ccmux llm proxy: pane is routed to codex account "+account.Name+", which cannot serve Anthropic API requests — clear the pane's llm route", http.StatusBadGateway)
			return
		}
		info := &reqInfo{account: account, pool: pool, pane: r.PathValue("pane"), rest: r.PathValue("rest")}
		rewriteRequest(r, account)
		if len(pool) > 1 {
			bufferForRetry(r, info)
		}
		proxy.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), infoKey{}, info)))
	})
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
