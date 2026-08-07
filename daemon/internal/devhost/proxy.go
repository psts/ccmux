package devhost

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strconv"
	"strings"
)

type portKey struct{}

// Handler wraps next with dev-hostname dispatch: a Host in the routing table
// reverse-proxies to its local port; an unrouted host under the dev domain
// gets a friendly 404 (so a typo doesn't land on the ccmux web lens); anything
// else falls through to next (the daemon API + web lens).
func (s *Server) Handler(next http.Handler) http.Handler {
	proxy := &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			port := pr.In.Context().Value(portKey{}).(int)
			pr.SetURL(&url.URL{Scheme: "http", Host: "127.0.0.1:" + strconv.Itoa(port)})
			pr.SetXForwarded()
			pr.Out.Host = pr.In.Host // dev servers vhost on the public name
		},
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			http.Error(w, fmt.Sprintf("ccmux devhost: %s is mapped but its dev server isn't answering (%v)", r.Host, err), http.StatusBadGateway)
		},
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// The lens alias is RESERVED: checked before the workspace table so a
		// later workspace claim can never shadow the settings UI you'd need to
		// fix the collision (the manager rejects such claims anyway).
		if lh := *s.lensHost.Load(); lh != "" && normalizeHost(r.Host) == lh {
			next.ServeHTTP(w, r)
			return
		}
		if port, ok := s.table.Load().Route(r.Host); ok {
			proxy.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), portKey{}, port)))
			return
		}
		if s.underDevDomain(r.Host) {
			s.unknownHost(w, r)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// underDevDomain reports whether host is inside the configured dev domain
// (always false in ts.net fallback mode — there, unrouted names never reach us).
func (s *Server) underDevDomain(host string) bool {
	domain := *s.domain.Load()
	return domain != "" && strings.HasSuffix(normalizeHost(host), "."+domain)
}

func (s *Server) unknownHost(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusNotFound)
	fmt.Fprintf(w, "ccmux devhost: no workspace maps %s\n\nknown hostnames:\n", r.Host)
	for _, h := range s.table.Load().Hosts() {
		fmt.Fprintf(w, "  https://%s\n", h)
	}
}
