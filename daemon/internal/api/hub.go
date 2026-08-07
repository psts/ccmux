package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"

	"github.com/gorilla/websocket"

	"ccmux.dev/ccmuxd/internal/hub"
)

// hubMode holds the hub-role dependencies: the member registry, the workspace
// aggregator (+ ownership index), the tailnet client (fetch + reverse proxy),
// the hub node's own MagicDNS label, and a tailnet WebSocket dialer used to fan
// in each host's event firehose. nil in host-only mode. See
// daemon/docs/multihost-plan.md.
type hubMode struct {
	reg    *hub.Registry
	agg    *hub.Aggregator
	client *hub.Client
	selfID string
	wsDial func(ctx context.Context, urlStr string) (*websocket.Conn, error)
}

// EnableHub switches the server into hub mode: GET /v1/workspaces aggregates
// every listing host, GET /v1/hosts exposes the registry, workspace/pane and
// /v1/hosts/{host} routes reverse-proxy to the owning host (self runs local),
// and GET /v1/events merges every host's firehose. wsDial must route the tailnet.
func (s *Server) EnableHub(reg *hub.Registry, agg *hub.Aggregator, client *hub.Client, selfID string, wsDial func(context.Context, string) (*websocket.Conn, error)) {
	s.hub = &hubMode{reg: reg, agg: agg, client: client, selfID: selfID, wsDial: wsDial}
}

// scoped applies hub owner-routing to a workspace/pane-{id}-scoped handler; in
// host-only mode it returns the handler unchanged.
func (s *Server) scoped(local http.HandlerFunc) http.HandlerFunc {
	if s.hub == nil {
		return local
	}
	return s.hub.ownerRoute(local)
}

// hostnamesRoute enforces GLOBAL dev-hostname uniqueness across every member
// host (hub mode) before delegating to the owner-routed handler: a label already
// claimed by a DIFFERENT workspace anywhere is rejected with which host holds it
// (the "pick anything, warn on collision" registrar). Off the hub it's unchanged.
func (s *Server) hostnamesRoute(local http.HandlerFunc) http.HandlerFunc {
	if s.hub == nil {
		return local
	}
	owned := s.hub.ownerRoute(local)
	return func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			writeError(w, http.StatusBadRequest, "read body: "+err.Error())
			return
		}
		r.Body = io.NopCloser(bytes.NewReader(body)) // let the downstream handler re-read
		if conflict := s.hub.hostnameConflict(r.PathValue("id"), body, s.mgr.LensHostname()); conflict != "" {
			writeError(w, http.StatusConflict, conflict)
			return
		}
		owned(w, r)
	}
}

// hostnameConflict returns a message when any requested dev-hostname label is
// already claimed by a DIFFERENT workspace on any host, else "". A malformed body
// passes through so the owner handler returns its own validation error.
func (h *hubMode) hostnameConflict(wsID string, body []byte, reserved string) string {
	var req struct {
		Hostnames []struct {
			Name string `json:"name"`
		} `json:"hostnames"`
	}
	if json.Unmarshal(body, &req) != nil {
		return ""
	}
	for _, hn := range req.Hostnames {
		if hn.Name == "" {
			continue
		}
		// The hub's lens alias: member daemons only check their own settings,
		// so the hub — the choke point every hostnames PUT proxies through —
		// enforces the fleet-wide reservation.
		if reserved != "" && hn.Name == reserved {
			return fmt.Sprintf("hostname %q is reserved for the ccmux web lens", reserved)
		}
		if host, ownerWs, ok := h.agg.HostnameOwner(hn.Name); ok && ownerWs != wsID {
			return fmt.Sprintf("hostname %q is already taken on host %s", hn.Name, host)
		}
	}
	return ""
}

// listHosts serves GET /v1/hosts: the federation registry, self first.
func (h *hubMode) listHosts(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, h.reg.List())
}

// listWorkspaces serves the aggregated GET /v1/workspaces (local + every listing
// remote host, host-stamped).
func (h *hubMode) listWorkspaces(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, h.agg.Aggregate(r.Context()))
}

// ownerRoute wraps a {id}-scoped handler: local when the hub owns the id,
// otherwise proxy to the owning host (compat-gated). The owner index covers both
// workspace and pane ids, so pane routes resolve too.
func (h *hubMode) ownerRoute(local http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		hostID, ok := h.agg.OwnerOrRefresh(r.Context(), id)
		if !ok {
			writeError(w, http.StatusNotFound, "unknown id "+id)
			return
		}
		if hostID == h.selfID {
			local(w, r)
			return
		}
		host, ok := h.reg.Get(hostID)
		if !ok {
			writeError(w, http.StatusBadGateway, "owning host "+hostID+" left the registry")
			return
		}
		if !h.allow(w, r, host) {
			return
		}
		h.client.ReverseProxy(host).ServeHTTP(w, r)
	}
}

// hostScoped wraps an explicit /v1/hosts/{host}/... route: self runs local,
// otherwise proxy to the named host with the path rewritten to targetPath.
func (h *hubMode) hostScoped(local http.HandlerFunc, targetPath string) http.HandlerFunc {
	return h.hostScopedWith(local, targetPath, true)
}

// hostScopedUpgrade is hostScoped WITHOUT the compat gate: a degraded host —
// one whose contract lags the hub's — is exactly the host a remote upgrade
// exists to fix, and `allow`'s refusal even says "upgrade the host". Health is
// the only precondition; the host-side handler re-validates everything else.
func (h *hubMode) hostScopedUpgrade(local http.HandlerFunc, targetPath string) http.HandlerFunc {
	return h.hostScopedWith(local, targetPath, false)
}

func (h *hubMode) hostScopedWith(local http.HandlerFunc, targetPath string, gated bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		hostID := r.PathValue("host")
		if hostID == h.selfID {
			local(w, r)
			return
		}
		host, ok := h.reg.Get(hostID)
		if !ok {
			writeError(w, http.StatusNotFound, "unknown host "+hostID)
			return
		}
		if gated && !h.allow(w, r, host) {
			return
		}
		r.URL.Path = targetPath
		h.client.ReverseProxy(host).ServeHTTP(w, r)
	}
}

// WrapDevhost, in hub mode, catches a dev-hostname request owned by a REMOTE
// member host and reverse-proxies it there over the tailnet — the hub terminates
// the wildcard TLS (its existing devhost cert), the owner serves its localhost
// port. Locally-owned hostnames and the API fall through to next. No-op off the
// hub, so single-host dev serving is unchanged. See daemon/docs/multihost-plan.md
// §4 (the wildcard A-record → hub and a shared devDomain across hosts are the live
// config that makes the proxied request reachable/servable).
func (s *Server) WrapDevhost(next http.Handler) http.Handler {
	if s.hub == nil {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		host := hostOnly(r.Host)
		// The lens alias is reserved FLEET-wide: never owner-route it to a
		// member host, even if one claims the label — this wrapper runs outside
		// the local devhost dispatch, so without this check a member's claim
		// would shadow the hub's own lens at exactly the URL you'd use to fix it.
		if lens := s.mgr.LensHostname(); lens != "" && devLabel(host, s.mgr.DevDomain()) == lens {
			next.ServeHTTP(w, r)
			return
		}
		if he, ok := s.hub.remoteDevTarget(host, s.mgr.DevDomain()); ok {
			s.hub.client.ReverseProxy(he).ServeHTTP(w, r)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// remoteDevTarget returns the member host that should serve a dev-hostname request
// for `host` (under `suffix`), when that host is a REMOTE, serving member. ok is
// false for a locally-owned, unknown, non-dev, or non-serving target.
func (h *hubMode) remoteDevTarget(host, suffix string) (hub.Host, bool) {
	label := devLabel(host, suffix)
	if label == "" {
		return hub.Host{}, false
	}
	owner, _, ok := h.agg.HostnameOwner(label)
	if !ok || owner == h.selfID {
		return hub.Host{}, false
	}
	he, ok := h.reg.Get(owner)
	if !ok || !he.Serves() {
		return hub.Host{}, false
	}
	return he, true
}

// devLabel extracts the single dev-hostname label from a Host header under a dev
// domain suffix ("app.dev.foo.io" + "dev.foo.io" → "app"); "" if it isn't a
// direct <label>.<suffix>.
func devLabel(host, suffix string) string {
	if suffix == "" {
		return ""
	}
	label, ok := strings.CutSuffix(host, "."+suffix)
	if !ok || label == "" || strings.Contains(label, ".") {
		return ""
	}
	return label
}

// hostOnly strips any :port from a Host header.
func hostOnly(hostport string) string {
	if h, _, err := net.SplitHostPort(hostport); err == nil {
		return h
	}
	return hostport
}

// allow enforces compat gating: ok hosts pass; degraded hosts pass GETs only
// (list-only + attach); unsupported/unreachable are refused with the reason.
func (h *hubMode) allow(w http.ResponseWriter, r *http.Request, host hub.Host) bool {
	if host.Serves() {
		return true
	}
	if host.Compat == hub.CompatDegraded && r.Method == http.MethodGet {
		return true
	}
	writeError(w, http.StatusConflict, fmt.Sprintf("host %s is %s: %s", host.ID, host.Compat, host.Reason))
	return false
}
