// Peers-bus HTTP surface. Mutating endpoints are double-gated: loopback-only
// AND bearer-token (from_id must match the token's identity) — the permission
// relay's honor-system trust model must never cross the tailnet boundary. The
// viewer surface (group history, group peers, listen-mode WS) is read-only and
// serves the tailnet like every other lens endpoint.
package api

import (
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"ccmux.dev/ccmuxd/internal/peers"
)

// EnablePeers wires the bus; without it every /v1/peers endpoint answers 503.
func (s *Server) EnablePeers(svc *peers.Service) { s.peersSvc = svc }

func (s *Server) peersEnabled(w http.ResponseWriter) bool {
	if s.peersSvc == nil {
		writeError(w, http.StatusServiceUnavailable, "peers bus disabled")
		return false
	}
	return true
}

func bearerToken(r *http.Request) string {
	return strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
}

func requireLoopback(w http.ResponseWriter, r *http.Request) bool {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if ip := net.ParseIP(host); err == nil && ip != nil && ip.IsLoopback() {
		return true
	}
	writeError(w, http.StatusForbidden, "peers mutations are loopback-only")
	return false
}

// peerConnAllowed authorizes a peers-bus connection's ORIGIN. Single-host (no
// hub): loopback-only, exactly as before. Hub mode ALSO accepts connections from
// a discovered member host — a remote pane's thin-client dialing over the tailnet
// — so cross-host peers work. The bearer token still authorizes the specific peer
// on top of this (defense in depth: a member can only act as panes it holds
// hub-minted tokens for). A non-member tailnet node is rejected.
func (s *Server) peerConnAllowed(w http.ResponseWriter, r *http.Request) bool {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err == nil {
		if ip := net.ParseIP(host); ip != nil {
			if ip.IsLoopback() {
				return true
			}
			if s.hub != nil && s.hub.reg.IsMemberIP(host) {
				return true
			}
		}
	}
	writeError(w, http.StatusForbidden, "peers connection must be loopback or a member host")
	return false
}

// requirePeer authorizes acting AS peerID: an allowed origin plus a token bound
// to that peer's identity.
func (s *Server) requirePeer(w http.ResponseWriter, r *http.Request, peerID string) bool {
	if !s.peerConnAllowed(w, r) {
		return false
	}
	if peerID == "" || !s.peersSvc.AuthorizePeer(peerID, bearerToken(r)) {
		writeError(w, http.StatusUnauthorized, "invalid peer token")
		return false
	}
	return true
}

func (s *Server) peersRegister(w http.ResponseWriter, r *http.Request) {
	if !s.peersEnabled(w) || !s.peerConnAllowed(w, r) {
		return
	}
	var req peers.RegisterReq
	if !decodeJSON(w, r, &req) {
		return
	}
	if !s.peersSvc.AuthorizeRegister(req, bearerToken(r)) {
		writeError(w, http.StatusUnauthorized, "invalid peer token")
		return
	}
	writeJSON(w, http.StatusOK, s.peersSvc.Register(req))
}

// peersMintPaneToken issues the bearer token a pane's sessions authenticate
// with, over THIS daemon's secret — the hub-authority path a member host calls
// (over the tailnet) so its panes join the hub's bus without any secret being
// distributed. Gated by peerConnAllowed (loopback or member host); minting for
// an arbitrary pane id is a plan-accepted risk (a compromised member is already
// inside the trust boundary). See daemon/docs/multihost-plan.md.
func (s *Server) peersMintPaneToken(w http.ResponseWriter, r *http.Request) {
	if !s.peersEnabled(w) || !s.peerConnAllowed(w, r) {
		return
	}
	var req struct {
		PaneID string `json:"pane_id"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	if req.PaneID == "" {
		writeError(w, http.StatusBadRequest, "pane_id required")
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"token": s.peersSvc.MintPaneToken(req.PaneID)})
}

func (s *Server) peersSend(w http.ResponseWriter, r *http.Request) {
	if !s.peersEnabled(w) {
		return
	}
	var req peers.SendReq
	if !decodeJSON(w, r, &req) {
		return
	}
	if !s.requirePeer(w, r, req.FromID) {
		return
	}
	writeJSON(w, http.StatusOK, s.peersSvc.Send(req))
}

type peerIDReq struct {
	PeerID string `json:"peer_id"`
	Scope  string `json:"scope"`
	Group  string `json:"group"`
}

func (s *Server) peersList(w http.ResponseWriter, r *http.Request) {
	if !s.peersEnabled(w) {
		return
	}
	var req peerIDReq
	if !decodeJSON(w, r, &req) {
		return
	}
	if !s.requirePeer(w, r, req.PeerID) {
		return
	}
	scope := req.Scope
	if scope == "" {
		scope = "project"
	}
	writeJSON(w, http.StatusOK, s.peersSvc.List(req.PeerID, scope, req.Group))
}

func (s *Server) peersSummary(w http.ResponseWriter, r *http.Request) {
	if !s.peersEnabled(w) {
		return
	}
	var req struct {
		PeerID  string `json:"peer_id"`
		Summary string `json:"summary"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	if !s.requirePeer(w, r, req.PeerID) {
		return
	}
	if !s.peersSvc.SetSummary(req.PeerID, req.Summary) {
		writeError(w, http.StatusNotFound, "unknown peer")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) peersUnregister(w http.ResponseWriter, r *http.Request) {
	if !s.peersEnabled(w) {
		return
	}
	var req peerIDReq
	if !decodeJSON(w, r, &req) {
		return
	}
	if !s.requirePeer(w, r, req.PeerID) {
		return
	}
	s.peersSvc.Unregister(req.PeerID)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) peersPoll(w http.ResponseWriter, r *http.Request) {
	if !s.peersEnabled(w) {
		return
	}
	var req peerIDReq
	if !decodeJSON(w, r, &req) {
		return
	}
	if !s.requirePeer(w, r, req.PeerID) {
		return
	}
	evs, err := s.peersSvc.Poll(req.PeerID)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	frames := make([]any, 0, len(evs))
	for _, ev := range evs {
		frames = append(frames, peers.WireFrame(ev))
	}
	writeJSON(w, http.StatusOK, map[string]any{"events": frames})
}

func (s *Server) peersPermissionRequest(w http.ResponseWriter, r *http.Request) {
	if !s.peersEnabled(w) {
		return
	}
	var req struct {
		PeerID       string `json:"peer_id"`
		RequestID    string `json:"request_id"`
		ToolName     string `json:"tool_name"`
		Description  string `json:"description"`
		InputPreview string `json:"input_preview"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	if !s.requirePeer(w, r, req.PeerID) {
		return
	}
	n, err := s.peersSvc.PermissionRequest(req.PeerID, req.RequestID, req.ToolName, req.Description, req.InputPreview)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "relayed_to": n})
}

// peersWS serves both socket modes: mode=peer (token-authed push channel with
// cursor replay + cumulative acks) and mode=listen (read-only per-group viewer
// stream, tailnet-OK).
func (s *Server) peersWS(w http.ResponseWriter, r *http.Request) {
	if !s.peersEnabled(w) {
		return
	}
	q := r.URL.Query()
	if q.Get("mode") == "listen" {
		group := q.Get("group")
		if group == "" {
			writeError(w, http.StatusBadRequest, "group required")
			return
		}
		conn, err := s.upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		s.peersSvc.AttachListener(group, conn)
		return
	}

	peerID := q.Get("peer_id")
	if !s.requirePeer(w, r, peerID) {
		return
	}
	conn, err := s.upgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	s.peersSvc.AttachPeer(peerID, conn)
}

// peersGroupMessages is the read-only history: GET /v1/peers/messages?group=&limit=&since=.
// since accepts RFC3339 or unix millis; default window 7 days, default limit 200.
func (s *Server) peersGroupMessages(w http.ResponseWriter, r *http.Request) {
	if !s.peersEnabled(w) {
		return
	}
	q := r.URL.Query()
	group := q.Get("group")
	if group == "" {
		writeError(w, http.StatusBadRequest, "group required")
		return
	}
	limit := 200
	if v, err := strconv.Atoi(q.Get("limit")); err == nil && v > 0 {
		limit = v
	}
	since := time.Now().Add(-7 * 24 * time.Hour).UnixMilli()
	if raw := q.Get("since"); raw != "" {
		if ts, err := time.Parse(time.RFC3339, raw); err == nil {
			since = ts.UnixMilli()
		} else if ms, err := strconv.ParseInt(raw, 10, 64); err == nil {
			since = ms
		}
	}
	msgs, err := s.peersSvc.GroupHistory(group, since, limit)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, msgs)
}

// peersLocalGroups receives the Mac app's live local-pane→window map (the
// window grouping source of truth for driver-mode panes the daemon doesn't
// host). Full-replace semantics; authed with the shared pane-less token.
func (s *Server) peersLocalGroups(w http.ResponseWriter, r *http.Request) {
	if !s.peersEnabled(w) || !requireLoopback(w, r) {
		return
	}
	if !s.peersSvc.AuthorizeLocalGroups(bearerToken(r)) {
		writeError(w, http.StatusUnauthorized, "invalid token")
		return
	}
	var req struct {
		Groups map[string]string `json:"groups"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	s.peersSvc.SetLocalPaneGroups(req.Groups)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "count": len(req.Groups)})
}

// peersGroupPeers is the read-only peers listing for viewers: GET /v1/peers?group=.
func (s *Server) peersGroupPeers(w http.ResponseWriter, r *http.Request) {
	if !s.peersEnabled(w) {
		return
	}
	group := r.URL.Query().Get("group")
	if group == "" {
		writeError(w, http.StatusBadRequest, "group required")
		return
	}
	writeJSON(w, http.StatusOK, s.peersSvc.GroupPeers(group))
}
