package devhost

import (
	"context"
	"net"
	"net/netip"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/caddyserver/certmagic"
	"github.com/libdns/cloudflare"
)

// State is the manager surface the devhost server reconciles against.
// *manager.Manager satisfies it.
type State interface {
	DevDomain() string
	CloudflareToken() string
	TailscaleAuthKey() string
	LensHostname() string
	AllHostnames() map[string]int
	StampHostnameRuntime(urlFor func(name string) string, listeningFor func(port int) bool)
}

// Server owns dev-hostname serving: it rebuilds the routing table from manager
// state on every Refresh and reconciles the active serving mode — a certmagic
// wildcard cert dispatched by SNI on the daemon's tsnet listener when a dev
// domain is configured, per-hostname tsnet nodes otherwise.
type Server struct {
	ctx      context.Context
	state    State
	dataDir  string     // certmagic storage + fallback tsnet node state live here
	tsSuffix string     // the tailnet's MagicDNS suffix (e.g. tailb9053d.ts.net)
	selfIP   netip.Addr // the daemon node's tailnet IPv4 — the A-record target

	table      atomic.Pointer[Table]
	domain     atomic.Pointer[string] // "" = ts.net fallback mode
	lensHost   atomic.Pointer[string] // full host serving the web lens ("" = off)
	certStatus atomic.Pointer[string]
	// magicCfg is the handshake-path view of the active certmagic config (nil
	// in fallback mode); separate from s.magic so TLS never takes s.mu.
	magicCfg atomic.Pointer[certmagic.Config]

	mu     sync.Mutex // guards reconcile state below (never taken on the request path)
	magic  *magicState
	nodes  map[string]*fallbackNode
	dnsKey string // (domain, token, ip) of the last successful A-record assert
	// newNode / newDNS are s.startNode and the Cloudflare provider in
	// production; fakes in tests (reconcile logic must never dial the
	// Tailscale control plane or the Cloudflare API).
	newNode func(name, authKey string) nodeHandle
	newDNS  func(token string) dnsProvider
}

// NewServer builds a devhost server. Call Refresh once at startup (after the
// daemon's tsnet node is up) and wire manager.OnDevhostChange to Refresh.
func NewServer(ctx context.Context, state State, dataDir, tsSuffix string, selfIP netip.Addr) *Server {
	s := &Server{ctx: ctx, state: state, dataDir: dataDir, tsSuffix: tsSuffix, selfIP: selfIP, nodes: map[string]*fallbackNode{}}
	s.newNode = s.startNode
	s.newDNS = func(token string) dnsProvider { return &cloudflare.Provider{APIToken: token} }
	empty, unset := "", "unset"
	s.table.Store(NewTable(nil))
	s.domain.Store(&empty)
	s.lensHost.Store(&empty)
	s.certStatus.Store(&unset)
	return s
}

// Refresh re-reads manager state and reconciles: routing table, serving mode,
// certs, fallback nodes, and the runtime URL/Listening stamps. Safe to call
// from any goroutine; mutations arrive here via manager.OnDevhostChange.
func (s *Server) Refresh() {
	s.mu.Lock()
	domain := strings.ToLower(strings.TrimSpace(s.state.DevDomain()))
	names := s.state.AllHostnames()
	suffix := domain
	if domain == "" {
		suffix = s.tsSuffix
	}
	byHost := make(map[string]int, len(names))
	for name, port := range names {
		byHost[name+"."+suffix] = port
	}
	s.table.Store(NewTable(byHost))
	s.domain.Store(&domain)
	// The reserved lens alias only exists in custom-domain mode — the ts.net
	// fallback already serves the lens at the daemon node's own name.
	lens := ""
	if label := strings.ToLower(strings.TrimSpace(s.state.LensHostname())); label != "" && domain != "" {
		lens = label + "." + domain
	}
	s.lensHost.Store(&lens)
	if domain != "" {
		token := s.state.CloudflareToken()
		s.ensureCertLocked(domain, token)
		s.ensureDNSLocked(domain, token)
		s.stopNodesLocked()
	} else {
		s.teardownMagicLocked()
		s.reconcileNodesLocked(names)
	}
	s.mu.Unlock()
	s.stampRuntime()
}

// CertStatus reports the wildcard-cert lifecycle for the settings UI:
// unset | pending | ready | error: <cause>.
func (s *Server) CertStatus() string {
	if *s.domain.Load() == "" {
		return "unset"
	}
	return *s.certStatus.Load()
}

// urlFor maps a bare hostname label to its https URL under the active mode.
func (s *Server) urlFor(name string) string {
	suffix := *s.domain.Load()
	if suffix == "" {
		suffix = s.tsSuffix
	}
	return "https://" + name + "." + suffix
}

// probe reports whether a local port currently accepts TCP connections.
func probe(port int) bool {
	conn, err := net.DialTimeout("tcp", "127.0.0.1:"+strconv.Itoa(port), 200*time.Millisecond)
	if err != nil {
		return false
	}
	conn.Close()
	return true
}

func (s *Server) stampRuntime() { s.state.StampHostnameRuntime(s.urlFor, probe) }

// StartProbe re-stamps the Listening bits on an interval (the stamp only
// broadcasts when something changed, so quiet ticks are free). Mirrors the
// git collector's polling loop.
func (s *Server) StartProbe(interval time.Duration) {
	go func() {
		t := time.NewTicker(interval)
		defer t.Stop()
		for {
			select {
			case <-s.ctx.Done():
				return
			case <-t.C:
				if len(s.state.AllHostnames()) > 0 {
					s.stampRuntime()
				}
			}
		}
	}()
}
