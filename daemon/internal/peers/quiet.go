// Quiet notices: send_message with quiet=true. A notice is stored in the
// addressee's log but never pushed, typed into a pane or allowed to wake an
// agent. It reaches the addressee only alongside something that reaches it
// anyway: the next message delivered to it (folded into that message's
// text), the first prompt of its next start, its own check_messages, or a
// repo session's next prompt (the UserPromptSubmit hook asks for them).
// Each notice is handed over once: delivering it marks it.
package peers

import (
	"fmt"
	"log"
	"strings"
	"time"

	"ccmux.dev/ccmuxd/internal/model"
)

// quietNoticesHeader opens the block of notices handed over together.
const quietNoticesHeader = "[claude-peers quiet notices: sent while you were away or busy, no reply needed]"

// sendQuietLocked is Send for a quiet notice: resolve without starting
// anyone, check reach as for any message, then store it unpushed.
func (s *Service) sendQuietLocked(req SendReq, sender *Peer, senderGroup string) SendResp {
	target, errText := s.resolveQuietTargetLocked(req, sender, senderGroup)
	if target == nil {
		return SendResp{Error: errText}
	}
	if err := s.checkReachableLocked(req, sender, senderGroup, target); err != "" {
		return SendResp{Error: err}
	}
	ev := s.eventFromLocked(sender, target, senderGroup, req.Text)
	ev.Quiet = true
	ev.SentAt = s.Now().UnixMilli()
	if _, err := s.st.AppendPeerEvent(ev); err != nil {
		return SendResp{Error: err.Error()}
	}
	s.fanToListenersLocked(ev)
	return SendResp{OK: true, Queued: true}
}

// resolveQuietTargetLocked finds a quiet notice's addressee. By name it
// takes an away peer too (an asleep agent, an archived repo session),
// since a notice is for reading later; a present one wins, then the one
// seen last. It never spawns: a name that has never run here has no
// mailbox to wait in.
func (s *Service) resolveQuietTargetLocked(req SendReq, sender *Peer, senderGroup string) (*Peer, string) {
	if req.ToID != "" {
		if t := s.peers[req.ToID]; t != nil {
			return t, ""
		}
		return nil, fmt.Sprintf("Peer %s not found", req.ToID)
	}
	if req.ToName == "" {
		return nil, "Either to_id or to_name is required"
	}
	wantGroup := senderGroup
	if req.ToGroup != "" {
		wantGroup = req.ToGroup
	}
	if best := s.quietByNameLocked(req.ToName, sender.ID, wantGroup); best != nil {
		return best, ""
	}
	return nil, fmt.Sprintf("Peer %q not found in project: a quiet notice only waits for a session or agent that has run here. "+
		"Send it without quiet (spawn_if_missing=true) to start it", req.ToName)
}

// quietByNameLocked is the best peer named name in group, present or not.
func (s *Service) quietByNameLocked(name, senderID, group string) *Peer {
	var best *Peer
	for _, p := range s.peers {
		if p.Name != name || p.ID == senderID || !sameGroup(s.groupOfLocked(p), group) {
			continue
		}
		if best == nil || s.betterQuietTargetLocked(p, best) {
			best = p
		}
	}
	return best
}

func (s *Service) betterQuietTargetLocked(p, than *Peer) bool {
	if pp, tp := s.presentLocked(p), s.presentLocked(than); pp != tp {
		return pp
	}
	return p.LastSeenAt > than.LastSeenAt
}

// foldNoticesLocked appends the addressee's waiting notices to a message
// about to be delivered, and returns the last seq it took (0: none), to be
// marked once the message is stored. A store error leaves the message
// as it is: the notices keep waiting for the next delivery.
func (s *Service) foldNoticesLocked(ev *model.PeerEvent) int64 {
	if ev.Kind != model.PeerEventMessage || ev.Quiet || ev.ToID == "" {
		return 0
	}
	pending, err := s.st.PendingNotices(ev.ToID)
	if err != nil {
		log.Printf("peers: quiet notices for %s unreadable (%v); they wait for the next delivery", ev.ToID, err)
		return 0
	}
	if len(pending) == 0 {
		return 0
	}
	ev.Text += "\n\n" + renderNotices(pending)
	return pending[len(pending)-1].Seq
}

func (s *Service) markNoticesLocked(toID string, upto int64) {
	if upto == 0 {
		return
	}
	if err := s.st.MarkNoticesDelivered(toID, upto); err != nil {
		log.Printf("peers: quiet notices for %s delivered but not marked (%v); they may arrive twice", toID, err)
	}
}

// renderNotices is the block a set of notices is handed over as.
func renderNotices(evs []*model.PeerEvent) string {
	var b strings.Builder
	b.WriteString(quietNoticesHeader)
	for _, ev := range evs {
		at := time.UnixMilli(ev.SentAt).Local().Format("2006-01-02 15:04")
		fmt.Fprintf(&b, "\n\nfrom %s (id %s), %s:\n%s", ev.FromName, ev.FromID, at, ev.Text)
	}
	return b.String()
}

// NoticesForPane is the waiting notices of the peer registered on paneID,
// rendered, and the last seq they cover: an agent start folds the text into
// its first prompt and marks them (MarkNoticesForPane) only once the start
// went through, so a failed start loses nothing. "" and 0 when none wait.
func (s *Service) NoticesForPane(paneID string) (string, int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	pending, err := s.st.PendingNotices(derivedID(paneID))
	if err != nil {
		log.Printf("peers: quiet notices for pane %s unreadable (%v)", paneID, err)
		return "", 0
	}
	if len(pending) == 0 {
		return "", 0
	}
	return renderNotices(pending), pending[len(pending)-1].Seq
}

// MarkNoticesForPane marks the notices NoticesForPane returned as delivered.
func (s *Service) MarkNoticesForPane(paneID string, upto int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.markNoticesLocked(derivedID(paneID), upto)
}
