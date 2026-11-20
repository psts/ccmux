package api

import (
	"crypto/subtle"
	"log"
	"net"
	"net/http"
	"net/url"
	"path"
	"strings"
	"sync"
	"time"
)

// The gate on the daemon's API for callers on this machine.
//
// A session behind the sandbox's walls shares this machine's network: it
// reaches the daemon's port like any local process. Without a gate it
// could switch the sandbox off (PUT /v1/sandbox), start a session that
// runs outside the walls and prompt it, or type into a terminal. So every
// /v1 route asks a caller on this machine for the daemon's API token,
// which is in a file the walls hide (cmd/ccmuxd/apitoken.go). Refusing is
// the rule and the open routes are the list: a route added later is gated
// until someone decides otherwise.
//
// Who is not asked:
//
//   - A caller from another machine. It comes in over the tailnet, which
//     has named it, and a walled session cannot come from there. "This
//     machine" is loopback and every address of this host's own
//     interfaces: a host that is on the tailnet itself (as the person)
//     would otherwise be vouched for by the tailnet when a walled session
//     calls the daemon's tailnet name.
//   - What a session calls with its own pane's token: the bus
//     (/v1/peers/...), and the tools the bus shim and a session's runner
//     reach under /v1/panes/{id}/.... The handlers of the first check the
//     token themselves; the gate checks it for the second, which until now
//     took any caller on loopback that named a pane.
//   - The few routes that carry nothing (health, whoami) or have a token
//     of their own (the clipboard pipe).
//
// A browser on this machine gets the token once in a link (`ccmuxd
// lens-url`), and the gate turns it into a cookie, so the lenses' own
// requests and websockets need no change.

// localOpen are the routes a caller on this machine may use without the
// API token: they carry nothing worth a wall, or they check a token of
// their own (the bus routes each check the caller's pane token, the
// clipboard pipe its own).
var localOpen = map[string]bool{
	"GET /v1/health":                    true,
	"GET /v1/whoami":                    true,
	"POST /v1/clipboard":                true,
	"POST /v1/peers/register":           true,
	"POST /v1/peers/bus":                true,
	"POST /v1/peers/send":               true,
	"POST /v1/peers/list":               true,
	"POST /v1/peers/agents":             true,
	"POST /v1/peers/sessions":           true,
	"POST /v1/peers/summary":            true,
	"POST /v1/peers/unregister":         true,
	"POST /v1/peers/poll":               true,
	"POST /v1/peers/permission-request": true,
	"POST /v1/peers/tasks/delegate":     true,
	"POST /v1/peers/tasks/update":       true,
	"POST /v1/peers/tasks/list":         true,
	"GET /v1/peers/ws":                  true, // its peer mode only: see viewersSocket
}

// paneOpen are the routes under /v1/panes/{id}/ that something in a pane
// calls with no token at all: a harness's plugin (opencode's, from before
// panes had one) sends a pane's busy or idle mark and asks for the
// paragraph about its window, and the git hook that signs a commit with
// its driver (hooks/ccmux-coauthor.sh) asks who that is. None starts,
// prompts or changes anything that runs.
var paneOpen = map[string]bool{
	"POST agent-signal": true,
	"GET bus-context":   true,
	"GET driver":        true,
}

// paneOwn are the routes under /v1/panes/{id}/ that a session calls for
// itself. The caller must hold that pane's own token: they file tasks,
// set timed prompts and lead threads in the pane's name, and a timed
// prompt reaches its session as if a person had typed it.
var paneOwn = map[string]bool{
	"POST tasks":      true,
	"POST todo":       true,
	"POST schedules":  true,
	"POST thread":     true,
	"POST first-edit": true,
	"POST agent-ask":  true,
	"POST notices":    true,
	"POST page":       true,
}

// keyParam is the query field a link carries the API token in, and
// keyHeader the header a script may send it in instead of a bearer token.
const (
	keyParam  = "key"
	keyHeader = "X-Ccmux-Key"
)

// LensHost is the name a browser on this machine opens the lens under. A
// cookie belongs to a host name whatever the port, so one set for
// 127.0.0.1 would be sent to every dev server on this machine, a walled
// session's among them. Under a name of the lens's own that takes a page
// that sends the browser to this name on the dev server's port: narrower,
// not closed.
const LensHost = "ccmux.localhost"

// SetAPIToken switches the gate on: from now a caller on this machine
// needs token for every /v1 route that is not on the open list. A daemon
// always sets one (main); a server without one gates nothing, which is
// what a test's server is.
func (s *Server) SetAPIToken(token string) { s.apiToken = token }

// gate wraps the routes with the gate.
func (s *Server) gate(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.apiToken == "" || !s.own.local(r.RemoteAddr) {
			next.ServeHTTP(w, r)
			return
		}
		clean := path.Clean("/" + r.URL.Path)
		if !strings.HasPrefix(clean, "/v1/") {
			s.servePage(w, r, clean, next)
			return
		}
		if s.holdsKey(r) || s.localRouteOpen(r, clean) {
			next.ServeHTTP(w, r)
			return
		}
		s.refused.note(r.Method, clean)
		writeJSON(w, http.StatusUnauthorized, map[string]string{
			"error": "this caller is on the daemon's own machine and sent no API token: send the one in the daemon's api-token file as a bearer token (`ccmuxd api-token` prints it), or open the lens with the link `ccmuxd lens-url` prints",
			"code":  "local-key",
		})
	})
}

// gateLog says in the daemon's log what the gate refused, once a minute
// per route: a script of the person's that stopped working must be easy to
// find, and a walled session that hammers the API must not fill the log.
type gateLog struct {
	mu   sync.Mutex
	last map[string]time.Time
}

const (
	gateLogEvery = time.Minute
	gateLogKeeps = 256
)

func (g *gateLog) note(method, route string) {
	if len(route) > 120 {
		route = route[:120]
	}
	key := method + " " + route
	g.mu.Lock()
	defer g.mu.Unlock()
	now := time.Now()
	if at, ok := g.last[key]; ok && now.Sub(at) < gateLogEvery {
		return
	}
	if g.last == nil || len(g.last) >= gateLogKeeps {
		g.last = map[string]time.Time{}
	}
	g.last[key] = now
	log.Printf("api: refused %q from this machine: no API token (`ccmuxd api-token` prints it; a session behind the sandbox's walls cannot have it)", key)
}

// holdsKey reports whether a request carries the API token: as a bearer
// token, in the key header, or in the cookie the key link set.
func (s *Server) holdsKey(r *http.Request) bool {
	if s.isKey(bearerToken(r)) || s.isKey(r.Header.Get(keyHeader)) {
		return true
	}
	c, err := r.Cookie(keyCookie(r))
	return err == nil && s.isKey(c.Value)
}

func (s *Server) isKey(got string) bool {
	return got != "" && subtle.ConstantTimeCompare([]byte(got), []byte(s.apiToken)) == 1
}

// localRouteOpen reports whether a caller on this machine may use a /v1
// route without the API token. clean is the request's path, cleaned.
//
// Only a path that is its own clean form and was sent with nothing
// escaped is ever open. The mux matches a route on the path as it was
// sent, one escaped piece at a time, so "/v1/windows/w/agents/..%2F..%2F..%2Fhealth"
// is the route that starts an agent, while its decoded, cleaned form reads
// "/v1/health". The two readings agree on a plain path, and only there.
func (s *Server) localRouteOpen(r *http.Request, clean string) bool {
	if r.URL.RawPath != "" || r.URL.Path != clean {
		return false
	}
	if localOpen[r.Method+" "+clean] {
		return !viewersSocket(r, clean)
	}
	if s.hubBus != nil && strings.HasPrefix(clean, HubBusPrefix+"/") {
		return true // the relay to the hub's bus checks the caller's token itself
	}
	return s.paneRouteOpen(r, clean)
}

// viewersSocket reports whether a request is for the bus socket's listen
// mode: the read-only stream of every message in a group, which the
// handler gives to whoever asks (peersWS; its other mode wants the peer's
// token). That is a lens's view, like GET /v1/peers/messages beside it, so
// from this machine it needs the API token: the socket's path is open for
// the mode a session's shim uses, and only that one.
func viewersSocket(r *http.Request, clean string) bool {
	return clean == "/v1/peers/ws" && r.URL.Query().Get("mode") == "listen"
}

// paneRouteOpen reports whether a route under /v1/panes/{id}/ is open to
// this caller: one a plugin calls with no token, or one of the pane's own
// tools, called with that pane's own token.
func (s *Server) paneRouteOpen(r *http.Request, clean string) bool {
	rest, ok := strings.CutPrefix(clean, "/v1/panes/")
	if !ok {
		return false
	}
	pane, tail, ok := strings.Cut(rest, "/")
	if !ok || pane == "" {
		return false
	}
	route := r.Method + " " + tail
	if paneOpen[route] {
		return true
	}
	return paneOwn[route] && s.peersSvc != nil && s.peersSvc.AuthorizePane(pane, bearerToken(r))
}

// takeKeyLink handles the link a browser on this machine is given: a page
// address with the API token in it. A right token becomes a cookie and the
// browser is sent on to the same page without it, so the token stays out
// of the address bar and the history. It reports whether it answered. Only
// a page's address is ever a key link (servePage): "key" is a field other
// routes use for their own ends.
func (s *Server) takeKeyLink(w http.ResponseWriter, r *http.Request) bool {
	if !r.URL.Query().Has(keyParam) {
		return false
	}
	if !s.isKey(r.URL.Query().Get(keyParam)) {
		http.Error(w, "ccmux: that link's key is not this daemon's. It changes every time the daemon starts: run `ccmuxd lens-url` again and open the link it prints.", http.StatusUnauthorized)
		return true
	}
	if to := lensHostURL(r); to != "" {
		// Under any other name the cookie would be one a dev server on this
		// machine is sent too: go to the lens's own name, key and all.
		http.Redirect(w, r, to, http.StatusSeeOther)
		return true
	}
	http.SetCookie(w, &http.Cookie{
		Name: keyCookie(r), Value: s.apiToken, Path: "/", HttpOnly: true, SameSite: http.SameSiteStrictMode,
		Secure: r.TLS != nil, MaxAge: int((30 * 24 * time.Hour).Seconds()),
	})
	q := r.URL.Query()
	q.Del(keyParam)
	to := url.URL{Path: r.URL.Path, RawQuery: q.Encode()}
	http.Redirect(w, r, to.String(), http.StatusSeeOther)
	return true
}

// lensHostURL is where a key link opened under a plain loopback name is
// sent instead: the same address under the lens's own name. "" when the
// request is under that name already, or under one that is nobody else's
// (the daemon's tailnet name).
func lensHostURL(r *http.Request) string {
	host, port, err := net.SplitHostPort(r.Host)
	if err != nil {
		host, port = r.Host, ""
	}
	if ip := net.ParseIP(host); host != "localhost" && (ip == nil || !ip.IsLoopback()) {
		return ""
	}
	to := url.URL{Scheme: "http", Host: LensHost, Path: r.URL.Path, RawQuery: r.URL.RawQuery}
	if port != "" {
		to.Host = net.JoinHostPort(LensHost, port)
	}
	return to.String()
}

// keyCookie is the name of the cookie that holds the API token for the
// address a request came in on. The port is in the name because a browser
// keeps one cookie jar per host name, not per port: two daemons on this
// machine (a scratch one beside the live one) would overwrite each other's.
func keyCookie(r *http.Request) string {
	_, port, err := net.SplitHostPort(r.Host)
	if err != nil || port == "" {
		port = "0"
	}
	return "ccmux_key_" + port
}

// servePage serves what is not the API: the lens's own files. They hold
// no secret and stay open, but a page opened on this machine without the
// key would only fail on its first request, so it is told how to get one
// instead.
func (s *Server) servePage(w http.ResponseWriter, r *http.Request, clean string, next http.Handler) {
	if r.Method != http.MethodGet || !(clean == "/" || strings.HasSuffix(clean, ".html")) {
		next.ServeHTTP(w, r)
		return
	}
	if s.takeKeyLink(w, r) {
		return
	}
	if s.holdsKey(r) {
		next.ServeHTTP(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusUnauthorized)
	_, _ = w.Write([]byte(keyPage))
}

// keyPage is what a browser on the daemon's own machine sees without the
// key.
const keyPage = `<!doctype html><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1">
<title>ccmux</title>
<body style="font:16px/1.5 system-ui,sans-serif;max-width:34rem;margin:15vh auto;padding:0 1rem">
<h1 style="font-size:1.25rem">This browser needs the daemon's key</h1>
<p>You opened ccmux from the machine its daemon runs on. Programs on that machine must show a key, so that work running in the sandbox cannot use this page's powers.</p>
<p>In a terminal on this machine, run:</p>
<pre style="background:#8882;padding:.75rem;border-radius:.4rem">ccmuxd lens-url</pre>
<p>and open the link it prints. The key changes every time the daemon starts.</p>
<p>From another device on your tailnet nothing changes: no key is needed there.</p>
`

// ownAddrs knows which callers are on this machine: loopback, and every
// address of this host's own interfaces. The list is read again when it is
// older than ownAddrsFor: an interface (the tailnet's) may come up after
// the daemon did.
type ownAddrs struct {
	mu     sync.Mutex
	at     time.Time
	ips    []net.IP
	failed bool
	// addrs lists the host's addresses; nil is net.InterfaceAddrs.
	addrs func() ([]net.Addr, error)
	now   func() time.Time
}

const ownAddrsFor = 15 * time.Second

// local reports whether a connection's far end is this machine. An address
// that cannot be read counts as local: the gate then asks for the token,
// which is the safe side.
func (o *ownAddrs) local(remoteAddr string) bool {
	host, _, err := net.SplitHostPort(remoteAddr)
	if err != nil {
		host = remoteAddr
	}
	// An address with a zone ("fe80::1%eth0") is a link-local one.
	host, _, _ = strings.Cut(host, "%")
	ip := net.ParseIP(host)
	if ip == nil || ip.IsLoopback() {
		return true
	}
	for _, own := range o.list() {
		if own.Equal(ip) {
			return true
		}
	}
	return false
}

// list is this host's own addresses, read at most every ownAddrsFor. When
// they cannot be read the last list stands, and the log says so once.
func (o *ownAddrs) list() []net.IP {
	o.mu.Lock()
	defer o.mu.Unlock()
	now := time.Now
	if o.now != nil {
		now = o.now
	}
	if !o.at.IsZero() && now().Sub(o.at) < ownAddrsFor {
		return o.ips
	}
	o.at = now()
	read := net.InterfaceAddrs
	if o.addrs != nil {
		read = o.addrs
	}
	addrs, err := read()
	if err != nil {
		if !o.failed {
			log.Printf("api: this host's own addresses could not be read, so a caller from one of them is not asked for the API token until they can: %v", err)
		}
		o.failed = true
		return o.ips
	}
	o.failed = false
	o.ips = o.ips[:0:0]
	for _, a := range addrs {
		if n, ok := a.(*net.IPNet); ok {
			o.ips = append(o.ips, n.IP)
		}
	}
	return o.ips
}
