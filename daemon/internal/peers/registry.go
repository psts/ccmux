// Peer registration, liveness, listing, and token authorization.
package peers

import (
	"crypto/hmac"
	"path/filepath"
	"strings"
	"syscall"
)

// RegisterReq is the thin client's registration payload. The daemon derives
// everything derivable (name, group, id) so clients stay dumb.
type RegisterReq struct {
	PaneID string `json:"pane_id"`
	// LocalPaneID identifies a Mac-local (driver-mode) pane, derived from
	// CCMUX_CMD_FILE; only meaningful when PaneID is empty.
	LocalPaneID string `json:"local_pane_id"`
	PID         int    `json:"pid"`
	CWD         string `json:"cwd"`
	GitRoot     string `json:"git_root"`
	Name        string `json:"name"`
	RequestedID string `json:"requested_id"`
	Summary     string `json:"summary"`
	// PollOnly marks a session that opted out of live push
	// (CCMUX_PEERS_CHANNEL=0): it collects messages by polling instead of
	// holding a socket. Sent as the NEGATIVE so an older client, which sends
	// nothing, defaults to push — the mode almost every session runs in.
	PollOnly bool `json:"poll_only"`
	// ShimVersion is the connecting shim's own version ("" = a pre-0.3.0 shim
	// that predates the field). Diagnostic only today; it exists so a future
	// wire change can be gated on fact instead of absent-field inference —
	// which matters once federation puts shims and daemon on different hosts.
	ShimVersion string `json:"shim_version"`
}

// RegisterResp echoes the derived identity back so the client can render tool
// output without knowing the derivation rules.
type RegisterResp struct {
	PeerID string `json:"peer_id"`
	Name   string `json:"name"`
	Group  string `json:"group"`
}

// Register is idempotent: the same pane (or requested_id) gets the same peer
// id back, and re-registration replaces the record in place. Local callers use
// it; the HTTP layer uses RegisterFrom so a federated peer can be labelled with
// the host it came from.
func (s *Service) Register(req RegisterReq) RegisterResp { return s.RegisterFrom(req, "") }

// RegisterFrom is Register plus the connection's origin IP, which the hub turns
// into the peer's owning-host label when there is no pane to look one up by.
// The IP comes from the socket, never from the request body: a self-asserted
// host would let one member claim to be another.
func (s *Service) RegisterFrom(req RegisterReq, originIP string) RegisterResp {
	s.mu.Lock()
	defer s.mu.Unlock()

	name := req.Name
	if name == "" {
		base := req.GitRoot
		if base == "" {
			base = req.CWD
		}
		name = filepath.Base(base)
	}

	// The owning host, resolved before anything compares pids: on the hub, two
	// member hosts each have a pid 1234, and every pid rule below is only true
	// within one host.
	host := s.originHostLocked(req, originIP)

	id := s.assignIDLocked(req)
	// A pane-less MCP server restarting in the same terminal re-registers with
	// a new peer id but the same pid; drop the stale record so it can't linger.
	// A record carrying a DIFFERENT local-pane identity is someone else's —
	// never evict it on pid alone. Dropping takes the mailbox with it: the old
	// id was random, so nothing can re-derive it and nothing could ever collect
	// that queue — leaving the cursor behind is how orphans were born.
	for otherID, p := range s.peers {
		if p.PaneID == "" && p.PID == req.PID && otherID != id && p.Host == host &&
			(p.LocalPaneID == "" || strings.EqualFold(p.LocalPaneID, req.LocalPaneID)) {
			s.dropPeerLocked(otherID)
		}
	}

	summary := req.Summary
	if prev := s.peers[id]; prev != nil && summary == "" {
		summary = prev.Summary
	}
	now := s.Now().UnixMilli()
	peer := &Peer{
		ID: id, Name: name, PaneID: req.PaneID, LocalPaneID: req.LocalPaneID,
		PID: req.PID, CWD: req.CWD, GitRoot: req.GitRoot, Summary: summary,
		RegisteredAt: now, LastSeenAt: now, PollOnly: req.PollOnly,
		ShimVersion: req.ShimVersion,
	}
	peer.Host = host
	s.peers[id] = peer
	// Record what this mailbox hangs off, so the collector can tell a mailbox
	// waiting for a returning session from one whose pane is long gone.
	_ = s.st.TouchPeerMailbox(id, substrateKey(peer), now)

	s.fulfillPendingSpawnLocked(peer)
	return RegisterResp{PeerID: id, Name: name, Group: s.groupOfLocked(peer)}
}

// originHostLocked names the host a registration belongs to, "" off the hub (or
// for a peer this hub owns itself). A pane answers through the federated pane
// map; a PANE-LESS session has no pane, so the connection's address answers —
// from the hub's own discovery, never from the request body.
//
// The label is load-bearing beyond display: every pid rule in the bus (the
// reaper's kill(0), the stale-record eviction) is meaningful only within one
// machine, and this is what says which machine that is.
func (s *Service) originHostLocked(req RegisterReq, originIP string) string {
	if s.hostForPane != nil && req.PaneID != "" {
		if h, ok := s.hostForPane(req.PaneID); ok {
			return h
		}
	}
	if req.PaneID == "" && originIP != "" && s.hostForAddr != nil {
		if h, ok := s.hostForAddr(originIP); ok {
			return h
		}
	}
	return ""
}

func (s *Service) assignIDLocked(req RegisterReq) string {
	if req.PaneID != "" {
		id := derivedID(req.PaneID)
		if p := s.peers[id]; p == nil || p.PaneID == req.PaneID {
			return id
		}
		return randomID() // hash collision with a different pane — vanishingly rare
	}
	if req.LocalPaneID != "" {
		// Mac-local panes get the same stable-by-construction ids as hosted ones.
		id := derivedID("local:" + strings.ToLower(req.LocalPaneID))
		if p := s.peers[id]; p == nil || strings.EqualFold(p.LocalPaneID, req.LocalPaneID) {
			return id
		}
		return randomID()
	}
	if req.RequestedID != "" {
		// Free, ours already, or held by a dead peer → honor the request.
		if p := s.peers[req.RequestedID]; p == nil || p.PID == req.PID || !s.substrateAliveLocked(p) {
			return req.RequestedID
		}
	}
	return randomID()
}

// Unregister handles a thin client leaving (stdin EOF). A pane peer whose pane
// still exists keeps its record and durable queue — its id is derived from the
// pane, so a message sent while its Claude session restarts is queued and
// replays when the session returns (the "restart later and still get it"
// guarantee). It is marked AWAY, though: it stops being present the instant it
// leaves, so no peer is ever shown a session that isn't there. Pane-less peers
// (where the process IS the client) and pane peers whose pane is gone are
// removed outright, mailbox and all.
func (s *Service) Unregister(peerID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if c := s.conns[peerID]; c != nil {
		c.close()
		delete(s.conns, peerID)
	}
	p := s.peers[peerID]
	if p == nil {
		return
	}
	// Only a CONFIRMED-gone pane forfeits the mailbox; an unresolvable pane is
	// assumed to be a blip and kept, because the reaper will confirm it later
	// and erasing it here would be irreversible.
	if substrateKey(p) != "" && !s.substrateGoneLocked(p) {
		p.AwayAt = s.Now().UnixMilli()
		return
	}
	s.dropPeerLocked(peerID)
}

// dropPeerLocked removes a peer AND erases its durable mailbox. Called only
// when the thing the mailbox hangs off is gone (the pane deleted, or a
// pane-less client's process exited), so no session could ever collect the
// queue — leaving it behind is the leak, not a safety net.
func (s *Service) dropPeerLocked(peerID string) {
	if c := s.conns[peerID]; c != nil {
		c.close()
		delete(s.conns, peerID)
	}
	if p := s.peers[peerID]; p != nil && p.PaneID != "" {
		s.forgetPaneSessionsLocked(p.PaneID)
	}
	delete(s.peers, peerID)
	_ = s.st.DeletePeerState(peerID)
}

// touchLocked records that a peer's session just proved it is there, which also
// cancels any away mark (a session that came back is present again).
func (s *Service) touchLocked(peerID string) {
	if p := s.peers[peerID]; p != nil {
		p.LastSeenAt = s.Now().UnixMilli()
		p.AwayAt = 0
	}
}

func (s *Service) touch(peerID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.touchLocked(peerID)
}

// SetSummary updates a peer's work summary.
func (s *Service) SetSummary(peerID, summary string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	p := s.peers[peerID]
	if p == nil {
		return false
	}
	p.Summary = summary
	s.touchLocked(peerID)
	return true
}

// ListEntry is one row of a peer listing. project duplicates group so the Mac
// overlay's existing decoder keeps working during the cutover.
type ListEntry struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Group     string `json:"group"`
	Project   string `json:"project"`
	PID       int    `json:"pid"`
	CWD       string `json:"cwd"`
	GitRoot   string `json:"git_root"`
	Summary   string `json:"summary"`
	LastSeen  string `json:"last_seen"`
	Connected bool   `json:"connected"`
	// PollOnly says this peer never holds a push socket, so Connected being
	// false is its normal healthy state rather than a broken connection.
	PollOnly bool `json:"poll_only,omitempty"`
	// Host is the peer's owning-host label (federation, hub mode); "" single-host.
	Host string `json:"host,omitempty"`
	// ShimVersion is the peer's connected shim ("" = pre-0.3.0): the readable
	// fact a federated wire mismatch gets diagnosed from.
	ShimVersion string `json:"shim_version,omitempty"`
}

// List returns the caller's visible peers for a scope: "project" (the caller's
// group — window or fallback), "directory" (same cwd), "repo" (same git
// root), or "all" (every peer on this daemon); group, when set, looks into
// another project instead of the caller's own. The caller itself is excluded.
// Only PRESENT peers are returned — a listed peer always has a session attached
// to it right now, so what a peer sees is what it can reach. Listing asks
// nothing about panes: a session that can still hold a socket open is reachable
// whatever the pane map currently says, and eviction is the reaper's job.
func (s *Service) List(callerID, scope, group string) []ListEntry {
	s.mu.Lock()
	defer s.mu.Unlock()
	caller := s.peers[callerID]
	if caller == nil {
		return nil
	}
	s.touchLocked(callerID)
	callerGroup := s.groupOfLocked(caller)
	// An explicit group is a deliberate look into another project — the
	// discovery half of to_group, so "who is running in ChartLabs?" is
	// answerable before you message anyone there.
	if group != "" {
		callerGroup = group
	}

	out := []ListEntry{}
	for _, p := range s.peers {
		if p.ID == callerID || !s.presentLocked(p) {
			continue
		}
		match := false
		switch scope {
		case "directory":
			match = p.CWD == caller.CWD
		case "repo":
			match = caller.GitRoot != "" && p.GitRoot == caller.GitRoot
		case "all", "machine":
			match = true
		default: // "project" and anything unrecognized: same group
			match = sameGroup(s.groupOfLocked(p), callerGroup)
		}
		if match {
			out = append(out, s.listEntryLocked(p))
		}
	}
	return out
}

// GroupPeers is the read-only viewer listing: every present peer in a group.
// Same presence rule as List — the lenses and the sessions must never disagree
// about who is there.
func (s *Service) GroupPeers(group string) []ListEntry {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := []ListEntry{}
	for _, p := range s.peers {
		if !s.presentLocked(p) {
			continue
		}
		if sameGroup(s.groupOfLocked(p), group) {
			out = append(out, s.listEntryLocked(p))
		}
	}
	return out
}

func (s *Service) listEntryLocked(p *Peer) ListEntry {
	g := s.groupOfLocked(p)
	return ListEntry{
		ID: p.ID, Name: p.Name, Group: g, Project: g,
		PID: p.PID, CWD: p.CWD, GitRoot: p.GitRoot, Summary: p.Summary,
		LastSeen: isoMillis(p.LastSeenAt), Connected: s.conns[p.ID] != nil,
		PollOnly: p.PollOnly, Host: p.Host, ShimVersion: p.ShimVersion,
	}
}

// presentLocked answers the only question a listing is allowed to answer: is
// there a Claude SESSION here that will read what you send it?
//
// A live socket is not that answer. ccmux-peers is a child of the claude
// process, not of the session, so it stays connected while Claude sits on the
// session picker with nothing running — online-looking, and unable to reply.
// Hooks settle it first; only then does the socket decide, and a peer that left
// cleanly or stopped proving itself within the grace window is not present.
func (s *Service) presentLocked(p *Peer) bool {
	if p.PaneID != "" && s.paneSessionDeadLocked(p.PaneID) {
		return false
	}
	if s.conns[p.ID] != nil {
		return true
	}
	if p.AwayAt != 0 {
		return false
	}
	return s.Now().UnixMilli()-p.LastSeenAt < presenceGrace.Milliseconds()
}

// substrateAliveLocked reports whether the thing a peer's mailbox hangs off
// still exists right now: its pane, or (pane-less) its MCP-server process.
// Deliberately independent of presence — a pane sitting at a shell prompt is
// alive but has nobody home, and conflating the two is what made departed
// sessions look online.
func (s *Service) substrateAliveLocked(p *Peer) bool {
	if p.PaneID != "" {
		return s.paneExistsLocked(p.PaneID)
	}
	if s.remoteLocked(p) {
		return s.presentLocked(p)
	}
	if p.PID <= 0 {
		return false
	}
	return syscall.Kill(p.PID, 0) == nil
}

// remoteLocked reports a pane-less peer living on ANOTHER host — the hub's view
// of a plain-terminal session on a member. Its pid indexes a process table this
// process cannot see, so kill(0) here is not a liveness test but a coin flip
// against an unrelated local process: it would keep a departed session listed
// because some daemon happens to hold that pid, or evict a live one because
// nothing does. Its own connection and heartbeat are the only honest evidence.
func (s *Service) remoteLocked(p *Peer) bool { return p.PaneID == "" && p.Host != "" }

// substrateKey is the durable identity a peer's mailbox hangs off — a hosted
// pane, or a Mac driver-mode pane. Both make the peer id re-derivable, which is
// what earns a mailbox the right to outlive its session. "" means the client
// process is the only thing behind it.
func substrateKey(p *Peer) string {
	if p.PaneID != "" {
		return p.PaneID
	}
	if p.LocalPaneID != "" {
		return "local:" + strings.ToLower(p.LocalPaneID)
	}
	return ""
}

// substrateGoneLocked is the erasure test — a single failed lookup is never
// enough. A pane must stay unresolvable for substrateGrace first (see
// keyGoneLocked); a process-only peer's dead pid is unambiguous and needs no
// confirmation, because kill(0) does not depend on any cache being warm.
func (s *Service) substrateGoneLocked(p *Peer) bool {
	if key := substrateKey(p); key != "" {
		return s.keyGoneLocked(key)
	}
	if s.remoteLocked(p) {
		// Presence, for the reason in remoteLocked. ReapOnce already drops any
		// pane-less peer that is not present, so this only keeps the two rules
		// from disagreeing about the same peer.
		return !s.presentLocked(p)
	}
	return !(p.PID > 0 && syscall.Kill(p.PID, 0) == nil)
}

// keyGoneLocked reports a substrate as gone only once it has failed to resolve
// for longer than substrateGrace. The hub rebuilds its federated pane map from
// scratch on every refresh, and a member host that fails one fetch drops all of
// its panes for that cycle; the Mac app's local-pane map is likewise empty until
// it re-pushes after a restart. Erasing mailboxes on either would destroy
// undelivered mail whose session is about to come back.
func (s *Service) keyGoneLocked(key string) bool {
	if s.substrateExistsLocked(key) {
		delete(s.missingSince, key)
		return false
	}
	now := s.Now().UnixMilli()
	first := s.missingSince[key]
	if first == 0 {
		s.missingSince[key] = now
		return false
	}
	return now-first >= substrateGrace.Milliseconds()
}

func (s *Service) substrateExistsLocked(key string) bool {
	if local, ok := strings.CutPrefix(key, "local:"); ok {
		_, found := s.localGroups[local]
		return found
	}
	return s.paneExistsLocked(key)
}

// paneExistsLocked resolves a pane anywhere the bus serves: this host's manager
// first, then (hub mode) the federation's aggregated pane map, so a peer on a
// member host is never mistaken for a deleted pane.
func (s *Service) paneExistsLocked(paneID string) bool {
	if _, ok := s.mgr.GroupForPane(paneID); ok {
		return true
	}
	if s.globalGroups != nil {
		if _, ok := s.globalGroups(paneID); ok {
			return true
		}
	}
	return false
}

// AuthorizeRegister checks a registration token: the pane token for pane
// sessions, the shared pane-less token otherwise.
func (s *Service) AuthorizeRegister(req RegisterReq, token string) bool {
	want := PanelessToken(s.secret)
	if req.PaneID != "" {
		want = TokenForPane(s.secret, req.PaneID)
	}
	return hmac.Equal([]byte(token), []byte(want))
}

// AuthorizePane checks a pane's own token, for callers that identify by pane id
// before any peer exists — the bus-resolution request a thin client makes on
// startup, before it has registered anywhere.
func (s *Service) AuthorizePane(paneID, token string) bool {
	if paneID == "" {
		return false
	}
	return hmac.Equal([]byte(token), []byte(TokenForPane(s.secret, paneID)))
}

// AuthorizeLocalGroups gates the Mac app's local-pane map push: it presents
// the shared pane-less token (readable only by the user via the 0600 info file).
func (s *Service) AuthorizeLocalGroups(token string) bool {
	return hmac.Equal([]byte(token), []byte(PanelessToken(s.secret)))
}

// AuthorizePaneless checks the shared pane-less credential — the counterpart to
// AuthorizePane for a session with no pane behind it, which is how a Claude
// started in a plain terminal proves itself when it asks which bus to join.
func (s *Service) AuthorizePaneless(token string) bool {
	return hmac.Equal([]byte(token), []byte(PanelessToken(s.secret)))
}

// AuthorizePeer checks that a token is valid for acting as an existing peer —
// from_id must match the token's identity.
func (s *Service) AuthorizePeer(peerID, token string) bool {
	s.mu.Lock()
	p := s.peers[peerID]
	s.mu.Unlock()
	if p == nil {
		return false
	}
	want := PanelessToken(s.secret)
	if p.PaneID != "" {
		want = TokenForPane(s.secret, p.PaneID)
	}
	return hmac.Equal([]byte(token), []byte(want))
}
