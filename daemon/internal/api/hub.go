package api

import (
	"context"
	"fmt"
	"net/http"

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
		if !h.allow(w, r, host) {
			return
		}
		r.URL.Path = targetPath
		h.client.ReverseProxy(host).ServeHTTP(w, r)
	}
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
