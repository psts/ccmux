// Peers-bus HTTP surface. Mutating endpoints are double-gated: loopback-only
// AND bearer-token (from_id must match the token's identity) — the permission
// relay's honor-system trust model must never cross the tailnet boundary. The
// viewer surface (group history, group peers, listen-mode WS) is read-only and
// serves the tailnet like every other lens endpoint.
package api

import (
	"log"
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

func isLoopback(r *http.Request) bool {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return false
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func requireLoopback(w http.ResponseWriter, r *http.Request) bool {
	if isLoopback(r) {
		return true
	}
	// Generic wording: this guard now also fronts /v1/clipboard — blaming
	// "peers" sent debuggers of a clipboard 403 to the wrong subsystem.
	writeError(w, http.StatusForbidden, "loopback-only endpoint")
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
	writeJSON(w, http.StatusOK, s.peersSvc.RegisterFrom(req, remoteIP(r)))
}

// remoteIP is the connection's address without its port, "" if unparsable. The
// bus labels a pane-less federated peer with the host this resolves to.
func remoteIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return ""
	}
	return host
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

// peersHostToken hands a member host the credential its PANE-LESS sessions
// register with — a Claude started in a plain terminal, which has no pane and
// therefore no pane token to mint against. Same authority path and same
// plan-accepted risk as peersMintPaneToken (a member is inside the trust
// boundary), and the same reason: no secret is distributed, the hub answers.
//
// The token never leaves the member's daemon. Its relay presents it upstream on
// behalf of a local caller that authenticated with the member's OWN pane-less
// token — see hubbus.go — so a local process gains no hub credential it could
// use directly.
func (s *Server) peersHostToken(w http.ResponseWriter, r *http.Request) {
	if !s.peersEnabled(w) || !s.peerConnAllowed(w, r) {
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"token": s.peersSvc.PanelessToken()})
}

// peersBus tells a pane's thin client WHICH bus to join, answered live from
// this host's tag:ccmux-hub discovery rather than from anything stamped into the
// pane's environment. That distinction is the whole point: pane env is written
// once at session creation and tmux sessions outlive daemon restarts by design,
// so a pane created before the hub existed stayed on its local bus forever —
// silently, and for weeks. Asking on every reconnect makes the tag the live
// authority it was meant to be.
//
// An empty url means "no hub discovered — stay on the daemon you asked". The
// caller already holds that URL and its token, so there is nothing to send.
func (s *Server) peersBus(w http.ResponseWriter, r *http.Request) {
	// requireLoopback, NOT peerConnAllowed: a pane asks its OWN daemon, so
	// unlike /v1/peers/pane-token there is no member host to admit — and this
	// route hands out a credential for a different bus. peerConnAllowed would
	// have accepted any member IP in hub mode, which is not what the contract
	// here says.
	if !s.peersEnabled(w) || !requireLoopback(w, r) {
		return
	}
	var req struct {
		PaneID string `json:"pane_id"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	// A PANE-LESS session — Claude started in a plain terminal — asks the same
	// question and deserves the same answer: it authenticates with the shared
	// pane-less token from the daemon-info file instead of a pane's. Leaving it
	// out is what kept those sessions marooned on their own host's bus while
	// every pane around them was on the hub's.
	authorized := s.peersSvc.AuthorizePane(req.PaneID, bearerToken(r))
	if req.PaneID == "" {
		authorized = s.peersSvc.AuthorizePaneless(bearerToken(r))
	}
	if !authorized {
		writeError(w, http.StatusUnauthorized, "invalid peer token")
		return
	}
	url, token := "", ""
	if s.busResolver != nil {
		var err error
		url, token, err = s.busResolver(req.PaneID)
		if err != nil {
			// 503, not an empty answer: the caller treats a successful empty
			// reply as "your own daemon is the bus" and would leave the hub.
			log.Printf("peers: bus resolve for %s: %v", orPaneless(req.PaneID), err)
			writeError(w, http.StatusServiceUnavailable, "bus unavailable")
			return
		}
	}
	writeJSON(w, http.StatusOK, map[string]string{"url": url, "token": token})
}

func orPaneless(paneID string) string {
	if paneID == "" {
		return "a pane-less session"
	}
	return "pane " + paneID
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

// peersDelegate is the tracked-send path: an ordinary bus message plus a
// durable task row whose updates flow back through the delegator's queue.
func (s *Server) peersDelegate(w http.ResponseWriter, r *http.Request) {
	if !s.peersEnabled(w) {
		return
	}
	var req peers.DelegateReq
	if !decodeJSON(w, r, &req) {
		return
	}
	if !s.requirePeer(w, r, req.FromID) {
		return
	}
	writeJSON(w, http.StatusOK, s.peersSvc.Delegate(req))
}

func (s *Server) peersTaskUpdate(w http.ResponseWriter, r *http.Request) {
	if !s.peersEnabled(w) {
		return
	}
	var req peers.TaskUpdateReq
	if !decodeJSON(w, r, &req) {
		return
	}
	if !s.requirePeer(w, r, req.PeerID) {
		return
	}
	writeJSON(w, http.StatusOK, s.peersSvc.UpdateTask(req))
}

func (s *Server) peersTasksList(w http.ResponseWriter, r *http.Request) {
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
	tasks, err := s.peersSvc.OpenTasks(req.PeerID)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"tasks": tasks})
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

// peersLocalGroups receives a live local-pane→window map (the window grouping
// source of truth for driver-mode panes the daemon doesn't host). Full-replace
// semantics, but only of the pushing host's own slice; authed with the shared
// pane-less token.
//
// Two callers: the Mac app over loopback, and — on the hub — a member daemon
// forwarding what its own app told it, so a driver-mode session that registered
// on the hub resolves to its window group. Hence peerConnAllowed rather than
// requireLoopback, matching how registrations already arrive.
func (s *Server) peersLocalGroups(w http.ResponseWriter, r *http.Request) {
	if !s.peersEnabled(w) || !s.peerConnAllowed(w, r) {
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
	// The pushing host is read off the connection, never the body: a member that
	// could name its own label could rewrite another member's grouping.
	//
	// A remote address that does not resolve is refused rather than defaulted.
	// peerConnAllowed and this lookup read the same member map, so they normally
	// agree; a registry refresh landing between the two is the gap, and treating
	// the miss as "local" would file that member's panes as this daemon's own and
	// delete its real ones. 503 because the next push, 60s later, will resolve.
	host, ok := s.peersSvc.HostForConn(remoteIP(r))
	if !ok {
		writeError(w, http.StatusServiceUnavailable, "pushing host not resolved")
		return
	}
	s.peersSvc.SetLocalPaneGroupsForHost(host, req.Groups)
	if host == "" && s.localGroupsSink != nil {
		// Ours to forward, and only ours — a map that arrived FROM a member is
		// already at its destination.
		//
		// Called directly, NOT on a goroutine. The sink hands the map to a
		// forwarder that serializes pushes latest-wins, and a goroutine per push
		// would let two of them reach it out of order — leaving the hub acting on
		// the older map, which is the exact thing the serialization is for. The
		// sink is written never to block a handler.
		s.localGroupsSink(req.Groups)
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "count": len(req.Groups)})
}

// peersViewerCredential answers a lens's two startup questions in one call:
// GET /v1/peers/viewer → {bus, token}.
//
// The two halves are gated differently, because they are not equally sensitive.
// WHICH BUS is not a secret — it says only whether this node federates onto a
// hub — and every lens needs it, including a browser, which has no way to hold a
// credential. So bus is always answered. The TOKEN unlocks a fleet-wide read and
// is handed out only to a caller that proves same-user by presenting the token
// from the 0600 daemon-info file, which a browser cannot do and the Mac app can.
//
// Address is deliberately NOT part of the test. An earlier version demanded the
// credential from loopback callers only, which vended the token to anyone who
// could reach this daemon from anywhere else — including an unprivileged local
// account on a shared host, which can arrive over the machine's tailscaled at
// this daemon's own tsnet node and so is not loopback. That handed the very
// caller the token exists to exclude a credential it could then replay.
//
// A lens that gets no token is NOT sent to the relay, because it could only be
// refused there. It is sent to the local routes with partial=true, which says
// "this is my own registry, not the fleet's" — the caveat a lens renders instead
// of claiming an empty panel means silence. Degraded and honest beats complete
// and wrong, but only if the lens is pointed somewhere it can actually read:
// naming the relay and withholding the key produced neither.
func (s *Server) peersViewerCredential(w http.ResponseWriter, r *http.Request) {
	if !s.peersEnabled(w) {
		return
	}
	// A caller that presents a WRONG credential is told so, rather than quietly
	// handed the tokenless answer. The Mac app clears its cached token on a 401,
	// and that re-mint is exactly what a caller holding a stale one needs.
	if presented := bearerToken(r); presented != "" && !s.peersSvc.AuthorizePaneless(presented) {
		log.Printf("peers: viewer credential refused (bad pane-less token) from %s", remoteIP(r))
		writeError(w, http.StatusUnauthorized, "invalid token")
		return
	}
	token := ""
	if s.peersSvc.AuthorizePaneless(bearerToken(r)) {
		token = s.peersSvc.ViewerToken()
	}
	bus := s.viewerBusPrefix()
	partial := bus != "" && token == ""
	if partial {
		bus = ""
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"bus":     bus,
		"token":   token,
		"partial": partial,
	})
}

// viewerBusPrefix is where a lens on THIS node should read the bus from.
func (s *Server) viewerBusPrefix() string {
	if s.busResolver == nil {
		return ""
	}
	url, _, err := s.busResolver("")
	if err != nil {
		// A hub this daemon knows about but could not reach just now. Answering
		// "" would send the lens to the local registry, which holds nobody once
		// sessions have federated — an empty panel reads as "nobody is here"
		// rather than as the fault it is. Point at the relay and let it say so.
		//
		// Logged because this is the only place the reason exists: the resolver
		// wraps a rotated hub secret and an unreachable hub into distinct errors,
		// and the lens sees the same sentence for both.
		log.Printf("peers: viewer bus resolve: %v — pointing the lens at the relay", err)
		return HubBusPrefix
	}
	if url != "" {
		return HubBusPrefix
	}
	return ""
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
