// The member-side hub-bus relay: a loopback path on THIS daemon that forwards
// peers-bus requests to the hub over the daemon's tailnet connection.
//
// A pane's thin client used to dial the hub itself, and on any host where
// ccmuxd runs its own tsnet node that cannot work: the daemon's tailnet identity
// (the tagged node the hub discovered) and the pane process's tailnet identity
// (the machine's own tailscaled, if it even has one) are DIFFERENT nodes with
// different IPs. The hub admits a remote pane only from a discovered member IP
// (peerConnAllowed), so every registration from such a host was rejected with
// "peers connection must be loopback or a member host" — silently, from the
// pane's side, forever.
//
// Relaying fixes that at the root: the pane talks only to 127.0.0.1, the hop to
// the hub carries the daemon's identity, and the hub's member-IP check keeps its
// full meaning instead of being weakened to accommodate a second identity. It
// also means a host needs no tailnet client of its own for peers to work.
package api

import (
	"fmt"
	"log"
	"net/http"
	"net/http/httputil"
	"net/url"
	"sync"
)

// HubBusPrefix is the loopback path the relay is mounted on. A pane's client
// treats the whole thing as its bus base URL and appends the same /v1/peers/…
// paths it would have sent to the hub, so nothing about the client changes.
const HubBusPrefix = "/v1/hubbus"

// ViewerTokenParam carries the lens credential on requests that cannot set a
// header. A browser's WebSocket constructor takes no headers at all, and the
// listen stream is the one viewer surface that must be a socket — so the token
// rides the query string for that hop and is stripped before the request
// crosses to the hub.
//
// That hop is whatever the lens page was loaded over: loopback, or the tailnet
// when the page came from this daemon's tsnet listener. A URL is one more place
// a credential can be logged, which is the cost of a constructor that takes no
// headers.
const ViewerTokenParam = "viewer_token"

// hubBus forwards to whatever hub is discovered RIGHT NOW. The target is read
// per request, not captured: hub discovery re-resolves tag:ccmux-hub every 15s,
// and a hub that moves must be followed without restarting the daemon.
type hubBus struct {
	hubURL    func() string
	transport http.RoundTripper
	upstream  func(inbound string) (string, error)

	mu     sync.Mutex
	target string
	proxy  *httputil.ReverseProxy
}

// SetHubBus arms the relay on a member host: hubURL reports the discovered hub
// ("" = none), and transport MUST route the tailnet (built from tsnet.Server.Dial)
// so the hop resolves MagicDNS and validates the hub's ts.net cert. Unset on a
// hub or a single-host node, where the route is not registered at all.
//
// upstream maps the bearer a local caller presented to the one the hub should
// see. A pane carries a hub-minted token already and passes through unchanged
// (""), a PANE-LESS session authenticates with this host's own shared token —
// which the hub knows nothing about — and the daemon substitutes the hub's. That
// keeps the hub's shared credential inside the daemon rather than handing it to
// every local process that can read the daemon-info file. An ERROR means the
// substitute could not be obtained, and the relay answers 503 instead of
// forwarding a token the hub will reject. nil = pass everything through.
func (s *Server) SetHubBus(hubURL func() string, transport http.RoundTripper, upstream func(string) (string, error)) {
	s.hubBus = &hubBus{hubURL: hubURL, transport: transport, upstream: upstream}
}

// relayKind is what a request IS, because the two halves of the bus surface
// answer to different credentials. Path alone cannot say: /v1/peers/ws is TWO
// handlers behind one path, chosen by ?mode=.
type relayKind int

const (
	// relayDenied is anything not on the allowlist, plus the peer arm of the
	// socket dialed with an explicit mode= (a thin client only ever dials it
	// with peer_id, so refusing the form it never sends costs it nothing).
	relayDenied relayKind = iota
	// relayClient is thin-client bus traffic, carrying a hub-minted pane token
	// or this host's shared one for the relay to substitute.
	relayClient
	// relayViewer is the read-only lens surface: group peers, group history,
	// and the listen stream.
	relayViewer
)

func relayKindFor(r *http.Request) relayKind {
	if viewerRelay(r) {
		return relayViewer
	}
	if !relayPaths[r.URL.Path] {
		return relayDenied
	}
	if r.URL.Path == "/v1/peers/ws" && r.URL.Query().Get("mode") != "" {
		return relayDenied
	}
	return relayClient
}

// viewerRelay names the lens read surface. On the hub these three are gated by
// tailnet reach and take no credential at all (peersWS upgrades the listen arm
// before checking anything). Relaying them on that basis would have put an
// unauthenticated read of every message, delegation and permission request in a
// guessable group name one dial away on every member host. So the relay demands
// a credential of its own — see viewerRelayRequest for which, and why the
// caller's address is not it.
func viewerRelay(r *http.Request) bool {
	switch r.URL.Path {
	case "/v1/peers", "/v1/peers/messages":
		return true
	case "/v1/peers/ws":
		return r.URL.Query().Get("mode") == "listen"
	}
	return false
}

// relayPaths is every path a thin client sends to its bus, and nothing else.
// Deliberately an allowlist: stripping a prefix and forwarding whatever remains
// would turn each member into a relay for the hub's WHOLE surface, reachable by
// any local process. A denylist would re-open that on the day someone adds a
// /v1/peers route to the hub, silently and with no test failing — and the route
// most worth protecting, /v1/peers/pane-token, mints a bus credential for an
// arbitrary pane id.
//
// Absent by design: /v1/peers/bus (this host's own question, answered locally),
// /v1/peers/pane-token (the hub-authority path the DAEMON calls, not a pane),
// and /v1/peers/local-groups (the Mac app's local pane map, which this daemon
// forwards to the hub itself rather than letting a local process address it).
// The lens read surface is NOT here: see viewerRelay, which admits it under a
// different credential.
var relayPaths = map[string]bool{
	"/v1/peers/register":           true,
	"/v1/peers/unregister":         true,
	"/v1/peers/send":               true,
	"/v1/peers/list":               true,
	"/v1/peers/summary":            true,
	"/v1/peers/poll":               true,
	"/v1/peers/permission-request": true,
	"/v1/peers/tasks/delegate":     true,
	"/v1/peers/tasks/update":       true,
	"/v1/peers/tasks/list":         true,
	"/v1/peers/ws":                 true,
}

// hubBusRelay serves HubBusPrefix/*. Mounted behind http.StripPrefix, so the
// path it sees is already the hub-side one.
func (s *Server) hubBusRelay(w http.ResponseWriter, r *http.Request) {
	kind := relayKindFor(r)
	if kind == relayDenied {
		writeError(w, http.StatusForbidden, "path is not relayable to the hub")
		return
	}
	// Bus traffic is loopback-only: it wears this host's identity upstream and
	// can register, send, and act as peers. A lens READ is admitted from any
	// address and gated on a credential instead — see viewerRelayRequest for why
	// the caller's address is not evidence of anything.
	if kind == relayClient && !requireLoopback(w, r) {
		return
	}
	target := s.hubBus.hubURL()
	if target == "" {
		// 503, not 404: "no hub right now" is a transient state a client should
		// retry through, and the route existing at all proves the relay is armed.
		writeError(w, http.StatusServiceUnavailable, "no hub discovered")
		return
	}
	if kind == relayViewer {
		if !s.viewerRelayRequest(w, r) {
			return
		}
	} else if err := s.hubBus.checkCredential(bearerToken(r)); err != nil {
		// Fail CLOSED. Forwarding a credential the hub is certain to reject turns
		// a reachability fault into "invalid peer token" in the caller's log —
		// which reads as a rotated secret, the one thing that is not wrong. Say
		// what actually happened, here, where the fault is.
		log.Printf("peers: hub relay cannot present a credential: %v", err)
		writeError(w, http.StatusServiceUnavailable, "hub credential unavailable")
		return
	}
	proxy, err := s.hubBus.proxyFor(target)
	if err != nil {
		log.Printf("peers: hub relay target %q unusable: %v", target, err)
		writeError(w, http.StatusServiceUnavailable, "hub address unusable")
		return
	}
	proxy.ServeHTTP(w, r)
}

// viewerRelayRequest authorizes a lens read and reports whether to forward it.
// The token may arrive as a bearer (the Mac app) or as a query parameter (a
// browser's WebSocket, which cannot set headers). Either way it is this host's
// own credential and is taken off the request before the hop, because the hub
// neither knows nor wants it.
//
// The credential is required from EVERY caller, whatever its address. An earlier
// version demanded it only from loopback, reasoning that anyone arriving over
// the tailnet could already read the hub directly. That reasoning does not hold:
// a daemon can be bound to a LAN address, an ACL can admit a node to members but
// not to the tag-isolated hub, and — decisively — an unprivileged local account
// on a shared host can reach this daemon's own tsnet node through the machine's
// tailscaled and so arrive here NOT from loopback. Address is not identity. The
// 0600 daemon-info file is the only same-user proof there is, so holding a token
// derived from it is the whole test.
func (s *Server) viewerRelayRequest(w http.ResponseWriter, r *http.Request) bool {
	if s.peersSvc == nil {
		writeError(w, http.StatusServiceUnavailable, "peers bus not enabled")
		return false
	}
	q := r.URL.Query()
	token := bearerToken(r)
	if token == "" {
		token = q.Get(ViewerTokenParam)
	}
	if !s.peersSvc.AuthorizeViewer(token) {
		writeError(w, http.StatusUnauthorized, "invalid viewer token")
		return false
	}
	if q.Has(ViewerTokenParam) {
		q.Del(ViewerTokenParam)
		r.URL.RawQuery = q.Encode()
	}
	// Dropped here, at the point of classification, rather than signalled to the
	// proxy's Director through the request context. The hub's viewer surface
	// takes no credential, and this one is a LOCAL secret that would mean nothing
	// there — so the read crosses anonymously.
	r.Header.Del("Authorization")
	return true
}

// checkCredential resolves the upstream bearer BEFORE proxying, so a failure to
// obtain one is answered as a failure rather than smuggled upstream as someone
// else's token. Nothing to translate (a pane's own hub-minted token, or no
// mapper wired) is not an error.
func (b *hubBus) checkCredential(inbound string) error {
	if b.upstream == nil {
		return nil
	}
	_, err := b.upstream(inbound)
	return err
}

// proxyFor returns the reverse proxy for a hub URL, rebuilt only when the hub
// changes. httputil.ReverseProxy carries WebSocket upgrades (/v1/peers/ws is a
// long-lived socket), which is why the relay is a proxy rather than a hand-rolled
// request copier.
func (b *hubBus) proxyFor(raw string) (*httputil.ReverseProxy, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return nil, err
	}
	if u.Scheme == "" || u.Host == "" {
		return nil, fmt.Errorf("hub url %q has no scheme or host", raw)
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.proxy != nil && b.target == raw {
		return b.proxy, nil
	}
	rp := httputil.NewSingleHostReverseProxy(&url.URL{Scheme: u.Scheme, Host: u.Host})
	rp.Transport = b.transport
	director := rp.Director
	upstream := b.upstream
	rp.Director = func(out *http.Request) {
		director(out)
		// The inbound Host is 127.0.0.1:<port>. The hub reads Host for its
		// dev-hostname dispatch, and a proxied request must look like it was
		// addressed to the hub, not to the pane's loopback.
		out.Host = u.Host
		if upstream == nil {
			return
		}
		// Errors are handled in hubBusRelay before we get here (fail closed); a
		// late failure would be a hub that vanished between the two, and leaving
		// the caller's bearer in place is what the pass-through case does anyway.
		if tok, err := upstream(bearerToken(out)); err == nil && tok != "" {
			out.Header.Set("Authorization", "Bearer "+tok)
		}
	}
	b.proxy, b.target = rp, raw
	return rp, nil
}
