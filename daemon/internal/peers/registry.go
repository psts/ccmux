// Peer registration, liveness, listing, and token authorization.
package peers

import (
	"crypto/hmac"
	"path/filepath"
	"syscall"
)

// RegisterReq is the thin client's registration payload. The daemon derives
// everything derivable (name, group, id) so clients stay dumb.
type RegisterReq struct {
	PaneID      string `json:"pane_id"`
	PID         int    `json:"pid"`
	CWD         string `json:"cwd"`
	GitRoot     string `json:"git_root"`
	Name        string `json:"name"`
	RequestedID string `json:"requested_id"`
	Summary     string `json:"summary"`
}

// RegisterResp echoes the derived identity back so the client can render tool
// output without knowing the derivation rules.
type RegisterResp struct {
	PeerID string `json:"peer_id"`
	Name   string `json:"name"`
	Group  string `json:"group"`
}

// Register is idempotent: the same pane (or requested_id) gets the same peer
// id back, and re-registration replaces the record in place.
func (s *Service) Register(req RegisterReq) RegisterResp {
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

	id := s.assignIDLocked(req)
	// A pane-less MCP server restarting in the same terminal re-registers with
	// a new peer id but the same pid; drop the stale record so it can't linger.
	for otherID, p := range s.peers {
		if p.PaneID == "" && p.PID == req.PID && otherID != id {
			delete(s.peers, otherID)
		}
	}

	summary := req.Summary
	if prev := s.peers[id]; prev != nil && summary == "" {
		summary = prev.Summary
	}
	peer := &Peer{
		ID: id, Name: name, PaneID: req.PaneID, PID: req.PID,
		CWD: req.CWD, GitRoot: req.GitRoot, Summary: summary,
		RegisteredAt: s.Now().UnixMilli(),
	}
	s.peers[id] = peer

	s.fulfillPendingSpawnLocked(peer)
	return RegisterResp{PeerID: id, Name: name, Group: s.groupOfLocked(peer)}
}

func (s *Service) assignIDLocked(req RegisterReq) string {
	if req.PaneID != "" {
		id := derivedID(req.PaneID)
		if p := s.peers[id]; p == nil || p.PaneID == req.PaneID {
			return id
		}
		return randomID() // hash collision with a different pane — vanishingly rare
	}
	if req.RequestedID != "" {
		// Free, ours already, or held by a dead peer → honor the request.
		if p := s.peers[req.RequestedID]; p == nil || p.PID == req.PID || !s.aliveLocked(p) {
			return req.RequestedID
		}
	}
	return randomID()
}

// Unregister drops a peer (thin client exiting on stdin EOF).
func (s *Service) Unregister(peerID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.peers, peerID)
	if c := s.conns[peerID]; c != nil {
		c.close()
		delete(s.conns, peerID)
	}
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
}

// List returns the caller's visible peers for a scope: "project" (the caller's
// group — window or fallback), "directory" (same cwd), "repo" (same git
// root), or "all" (every peer on this daemon). The caller itself is excluded.
// Dead peers are evicted here, so inclusion is the liveness signal.
func (s *Service) List(callerID, scope string) []ListEntry {
	s.mu.Lock()
	defer s.mu.Unlock()
	caller := s.peers[callerID]
	if caller == nil {
		return nil
	}
	callerGroup := s.groupOfLocked(caller)

	out := []ListEntry{}
	for id, p := range s.peers {
		if id == callerID {
			continue
		}
		if !s.aliveLocked(p) {
			delete(s.peers, id)
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
			match = s.groupOfLocked(p) == callerGroup
		}
		if match {
			out = append(out, s.listEntryLocked(p))
		}
	}
	return out
}

// GroupPeers is the read-only viewer listing: every live peer in a group.
func (s *Service) GroupPeers(group string) []ListEntry {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := []ListEntry{}
	for id, p := range s.peers {
		if !s.aliveLocked(p) {
			delete(s.peers, id)
			continue
		}
		if s.groupOfLocked(p) == group {
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
		LastSeen: isoMillis(p.RegisteredAt), Connected: s.conns[p.ID] != nil,
	}
}

// aliveLocked: a pane peer lives as long as its pane; a pane-less peer as long
// as its MCP-server process (kill 0 probe, the old broker's check).
func (s *Service) aliveLocked(p *Peer) bool {
	if p.PaneID != "" {
		_, ok := s.mgr.GroupForPane(p.PaneID)
		return ok
	}
	if p.PID <= 0 {
		return false
	}
	return syscall.Kill(p.PID, 0) == nil
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
