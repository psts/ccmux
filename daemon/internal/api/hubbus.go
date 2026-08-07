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

// hubBus forwards to whatever hub is discovered RIGHT NOW. The target is read
// per request, not captured: hub discovery re-resolves tag:ccmux-hub every 15s,
// and a hub that moves must be followed without restarting the daemon.
type hubBus struct {
	hubURL    func() string
	transport http.RoundTripper
	upstream  func(inbound string) string

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
// see. A pane carries a hub-minted token already and passes through unchanged; a
// PANE-LESS session authenticates with this host's own shared token, which the
// hub knows nothing about, and the daemon substitutes the hub's. That keeps the
// hub's shared credential inside the daemon rather than handing it to every
// local process that can read the daemon-info file. nil = pass everything
// through.
func (s *Server) SetHubBus(hubURL func() string, transport http.RoundTripper, upstream func(string) string) {
	s.hubBus = &hubBus{hubURL: hubURL, transport: transport, upstream: upstream}
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
// and /v1/peers/local-groups (the Mac app's local pane map).
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

func relayable(path string) bool { return relayPaths[path] }

// hubBusRelay serves HubBusPrefix/*. Mounted behind http.StripPrefix, so the
// path it sees is already the hub-side one.
func (s *Server) hubBusRelay(w http.ResponseWriter, r *http.Request) {
	if !requireLoopback(w, r) {
		return
	}
	if !relayable(r.URL.Path) {
		writeError(w, http.StatusForbidden, "path is not relayable to the hub")
		return
	}
	target := s.hubBus.hubURL()
	if target == "" {
		// 503, not 404: "no hub right now" is a transient state a client should
		// retry through, and the route existing at all proves the relay is armed.
		writeError(w, http.StatusServiceUnavailable, "no hub discovered")
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
		if tok := upstream(bearerToken(out)); tok != "" {
			out.Header.Set("Authorization", "Bearer "+tok)
		}
	}
	b.proxy, b.target = rp, raw
	return rp, nil
}
