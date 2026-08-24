package llmproxy

import (
	"context"
	"log"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
)

type reqInfo struct {
	account Account
	pane    string
	rest    string
}

type infoKey struct{}

// Handler serves the pane-scoped LLM routes. It expects to be mounted on
// patterns of the form `/llm/pane/{pane}/{rest...}` (the api server owns the
// mux and the loopback guard); {rest} is the upstream path, e.g. v1/messages.
func (s *Service) Handler() http.Handler {
	proxy := &httputil.ReverseProxy{
		Rewrite:      rewrite,
		ErrorHandler: upstreamError,
		// LLM responses stream as SSE; buffering a token stream would stall the
		// harness until the answer is complete.
		FlushInterval: -1,
		ModifyResponse: func(resp *http.Response) error {
			info := resp.Request.Context().Value(infoKey{}).(reqInfo)
			log.Printf("llm: pane %s → %s %d %s /%s", info.pane, info.account.Name, resp.StatusCode, resp.Request.Method, info.rest)
			return nil
		},
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		account, err := s.resolve(r.PathValue("pane"))
		if err != nil {
			http.Error(w, "ccmux llm proxy: "+err.Error(), http.StatusBadGateway)
			return
		}
		info := reqInfo{account: account, pane: r.PathValue("pane"), rest: r.PathValue("rest")}
		if needsSystemTurnCompat(account) {
			downgradeSystemTurns(r)
		}
		proxy.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), infoKey{}, info)))
	})
}

// rewrite points the outbound request at the account's upstream: the pane
// prefix is dropped, the client's auth is replaced only when the account
// holds a key (see Account).
func rewrite(pr *httputil.ProxyRequest) {
	info := pr.In.Context().Value(infoKey{}).(reqInfo)
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
// to Anthropic untouched.
func applyAuth(out *http.Request, a Account) {
	if a.APIKey == "" {
		return
	}
	out.Header.Del("Authorization")
	out.Header.Del("x-api-key")
	if a.Kind == "openai" {
		out.Header.Set("Authorization", "Bearer "+a.APIKey)
		return
	}
	out.Header.Set("x-api-key", a.APIKey)
}

func upstreamError(w http.ResponseWriter, r *http.Request, err error) {
	info, _ := r.Context().Value(infoKey{}).(reqInfo)
	msg := "ccmux llm proxy: upstream " + info.account.Name + " is not answering"
	if strings.Contains(err.Error(), "context canceled") {
		// The harness hung up first; nothing to report and nobody listening.
		return
	}
	log.Printf("llm: pane %s → %s failed: %v", info.pane, info.account.Name, err)
	http.Error(w, msg, http.StatusBadGateway)
}
