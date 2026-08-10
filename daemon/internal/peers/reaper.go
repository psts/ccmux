// Reaping: convergence between what the registry believes, what is actually
// running, and what the database holds. Eviction used to happen only inside
// List, so a peer nobody listed never expired and the database never heard
// about it at all — cursors accumulated as a permanent high-water mark of every
// session that ever existed. Here it runs on a timer instead, and every
// eviction carries the matching database delete with it.
package peers

import (
	"context"
	"time"
)

const (
	// presenceGrace bounds how long a peer stays listed after its socket drops
	// without a clean goodbye. It must exceed the thin client's worst-case
	// reconnect (15s backoff) and a poll-only session's re-register period
	// (60s), so a genuine flap is never mistaken for a departure. A clean exit
	// doesn't wait for it — that path marks the peer away immediately.
	presenceGrace = 150 * time.Second
	// reapInterval is how often the registry is swept for dead substrate.
	reapInterval = 15 * time.Second
	// gcInterval is how often orphaned mailboxes are erased from the database.
	gcInterval = time.Minute
	// gcStartupGrace holds collection off until surviving clients have had time
	// to re-register after a daemon restart. Until they do, the registry is
	// empty and (in hub mode) the federation's pane map is cold: deleting on
	// that evidence would erase live mailboxes and replay delivered mail.
	gcStartupGrace = 5 * time.Minute
	// substrateGrace is how long a pane must stay unresolvable before anything
	// hanging off it is erased. Erasure is irreversible and pane lookups are
	// cache-backed, so absence has to be corroborated across several cycles.
	substrateGrace = 2 * time.Minute
)

// startReaper runs the presence sweep and the mailbox collector for the
// lifetime of ctx.
func (s *Service) startReaper(ctx context.Context) {
	started := s.Now()
	reap := time.NewTicker(reapInterval)
	defer reap.Stop()
	gc := time.NewTicker(gcInterval)
	defer gc.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-reap.C:
			s.ReapOnce()
		case <-gc.C:
			if s.Now().Sub(started) >= gcStartupGrace {
				s.CollectMailboxes()
			}
		}
	}
}

// ReapOnce drops every peer whose substrate is confirmed gone — its pane
// deleted, or its pane-less client process exited — and erases the mailbox with
// it. Exported so tests can drive a sweep without waiting on the ticker.
func (s *Service) ReapOnce() {
	s.mu.Lock()
	defer s.mu.Unlock()
	for id, p := range s.peers {
		if s.substrateGoneLocked(p) {
			s.dropPeerLocked(id)
			continue
		}
		// A peer with no pane behind it is finished the moment it stops being
		// present, even though its process lingers: its id was random, so
		// nothing can re-derive it and no session can ever collect its queue.
		// Only pane-backed peers earn a standing mailbox. Without this, a client
		// whose goodbye failed to land — the POST is fire-and-forget, and it
		// forgets its id either way — would be registered forever.
		if substrateKey(p) == "" && !s.presentLocked(p) {
			s.dropPeerLocked(id)
		}
	}
}

// CollectMailboxes erases database state for mailboxes nothing can ever collect:
// a pane peer's whose pane is gone, and a pane-less peer's left behind by a
// daemon that died before its client could unregister. A mailbox is deleted only
// on POSITIVE evidence — a registered peer, or a pane the bus can still resolve,
// keeps it — so an unfamiliar pane is left alone rather than guessed away.
func (s *Service) CollectMailboxes() {
	boxes, err := s.st.PeerMailboxes()
	if err != nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, b := range boxes {
		if s.peers[b.PeerID] != nil {
			continue // a session holds it, present or merely away
		}
		if b.UpdatedAt == 0 {
			// Unknown provenance: written before mailboxes recorded a substrate,
			// and never re-registered since. An empty pane id here does NOT mean
			// "pane-less" — it means "we never wrote one down". Reading it as
			// pane-less erased three live panes' queues once already; ambiguity is
			// not evidence, and these rows are a fixed, shrinking set.
			continue
		}
		if b.PaneID != "" && !s.orphanKeyGoneLocked(b.PaneID) {
			continue // the pane can still host a returning session
		}
		_ = s.st.DeletePeerState(b.PeerID)
	}
}
