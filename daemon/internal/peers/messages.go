// Send routing, verdict interception, poll, and event delivery.
package peers

import (
	"fmt"
	"regexp"
	"strings"

	"ccmux.dev/ccmuxd/internal/model"
)

// permReplyRe matches a delegator's verdict reply: "yes abcde" / "no abcde"
// (request ids are five lowercase letters excluding 'l'). Matched centrally at
// route time and ONLY against outstanding requests — an id nobody asked about
// falls through as a normal message instead of silently vanishing.
var permReplyRe = regexp.MustCompile(`(?i)^\s*(y|yes|n|no)\s+([a-km-z]{5})\s*$`)

// SendReq mirrors the send_message tool's arguments.
type SendReq struct {
	FromID         string `json:"from_id"`
	ToID           string `json:"to_id"`
	ToName         string `json:"to_name"`
	Text           string `json:"text"`
	SpawnIfMissing bool   `json:"spawn_if_missing"`
	ToRepo         string `json:"to_repo"`
}

// SendResp is the tool-facing outcome. Error strings the old broker used are
// preserved verbatim — running sessions pattern-match on them.
type SendResp struct {
	OK       bool   `json:"ok"`
	Spawning bool   `json:"spawning,omitempty"`
	Error    string `json:"error,omitempty"`
}

// Send routes one message: resolve the target inside the sender's group,
// enforce the same-group guard, intercept permission verdicts, else append a
// message event and push it.
func (s *Service) Send(req SendReq) SendResp {
	s.mu.Lock()
	defer s.mu.Unlock()

	sender := s.peers[req.FromID]
	if sender == nil {
		return SendResp{Error: "sender is not registered"}
	}
	senderGroup := s.groupOfLocked(sender)

	target, resp := s.resolveTargetLocked(req, sender, senderGroup)
	if target == nil {
		return resp
	}
	if senderGroup != s.groupOfLocked(target) {
		return SendResp{Error: "Cannot send messages across projects"}
	}

	if handled, resp := s.tryVerdictLocked(sender, target, senderGroup, req.Text); handled {
		return resp
	}

	ev := s.eventFromLocked(sender, target, senderGroup, req.Text)
	if err := s.deliverLocked(ev); err != nil {
		return SendResp{Error: err.Error()}
	}
	return SendResp{OK: true}
}

// resolveTargetLocked finds the addressee: by id, or by name within the
// sender's group (erroring on ambiguity instead of the old silent first-match),
// optionally spawning a missing named teammate.
func (s *Service) resolveTargetLocked(req SendReq, sender *Peer, senderGroup string) (*Peer, SendResp) {
	if req.ToID != "" {
		t := s.peers[req.ToID]
		if t == nil {
			return nil, SendResp{Error: fmt.Sprintf("Peer %s not found", req.ToID)}
		}
		return t, SendResp{}
	}
	if req.ToName == "" {
		return nil, SendResp{Error: "Either to_id or to_name is required"}
	}
	var matches []*Peer
	for _, p := range s.peers {
		if p.Name == req.ToName && p.ID != sender.ID && s.groupOfLocked(p) == senderGroup {
			matches = append(matches, p)
		}
	}
	switch len(matches) {
	case 1:
		return matches[0], SendResp{}
	case 0:
		if req.SpawnIfMissing {
			return nil, s.trySpawnLocked(sender, senderGroup, req.ToName, req.Text, req.ToRepo)
		}
		return nil, SendResp{Error: fmt.Sprintf("Peer %q not found in project", req.ToName)}
	default:
		return nil, SendResp{Error: fmt.Sprintf(
			"Multiple peers named %q in this group — reply with to_id instead (see list_peers)", req.ToName)}
	}
}

// tryVerdictLocked converts a "yes <id>"/"no <id>" reply into a structured
// permission_verdict event when — and only when — the target has an
// outstanding request with that id. First valid verdict wins; later ones are
// dropped silently (today's client-side regex ate them too).
func (s *Service) tryVerdictLocked(sender, target *Peer, group, text string) (bool, SendResp) {
	m := permReplyRe.FindStringSubmatch(text)
	if m == nil {
		return false, SendResp{}
	}
	rid := strings.ToLower(m[2])
	pr := s.perms[rid]
	if pr == nil || pr.workerID != target.ID {
		return false, SendResp{} // not an outstanding request → normal message
	}
	if pr.resolved {
		return true, SendResp{OK: true}
	}
	pr.resolved = true
	behavior := "deny"
	if strings.HasPrefix(strings.ToLower(m[1]), "y") {
		behavior = "allow"
	}
	ev := s.eventFromLocked(sender, target, group, text)
	ev.Kind = model.PeerEventVerdict
	ev.RequestID = rid
	ev.Behavior = behavior
	if err := s.deliverLocked(ev); err != nil {
		return true, SendResp{Error: err.Error()}
	}
	return true, SendResp{OK: true}
}

// eventFromLocked snapshots the sender's display fields and the group into a
// message event, so history stays renderable after the sender leaves.
func (s *Service) eventFromLocked(sender, target *Peer, group, text string) *model.PeerEvent {
	return &model.PeerEvent{
		Kind:   model.PeerEventMessage,
		FromID: sender.ID, FromName: sender.Name,
		FromSummary: sender.Summary, FromCWD: sender.CWD,
		ToID: target.ID, ToName: target.Name,
		Group: group, Text: text,
	}
}

// systemEventLocked builds a bus-authored notice (spawn timeouts).
func systemEvent(target *Peer, group, text string) *model.PeerEvent {
	return &model.PeerEvent{
		Kind:   model.PeerEventMessage,
		FromID: "claude-peers", FromName: "claude-peers",
		ToID: target.ID, ToName: target.Name,
		Group: group, Text: text,
	}
}

// deliverLocked appends the event to the log and pushes it to the addressee's
// connection (if any) and the group's listeners. Appending inside the service
// lock makes append+push atomic relative to conn attachment, so an event is
// always either in a subscriber's replay or in its channel — never neither.
func (s *Service) deliverLocked(ev *model.PeerEvent) error {
	ev.SentAt = s.Now().UnixMilli()
	if _, err := s.st.AppendPeerEvent(ev); err != nil {
		return err
	}
	if c := s.conns[ev.ToID]; c != nil {
		c.enqueue(ev)
	}
	s.fanToListenersLocked(ev)
	return nil
}

// Poll returns a peer's events past its cursor and advances the cursor — the
// check_messages path, and the sole delivery path for a session running
// without live channel push. One shared cursor with the push path means an
// already-pushed-and-acked message never re-appears here.
func (s *Service) Poll(peerID string) ([]*model.PeerEvent, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.peers[peerID] == nil {
		return nil, fmt.Errorf("peer %s is not registered", peerID)
	}
	cursor, err := s.st.PeerCursor(peerID)
	if err != nil {
		return nil, err
	}
	evs, err := s.st.PeerEventsAfter(peerID, cursor)
	if err != nil {
		return nil, err
	}
	if len(evs) > 0 {
		if err := s.st.AdvancePeerCursor(peerID, evs[len(evs)-1].Seq); err != nil {
			return nil, err
		}
	}
	return evs, nil
}

// GroupHistory is the read-only viewer's message list, oldest first, with
// names resolved live-peer-first (snapshot fallback for departed peers).
func (s *Service) GroupHistory(group string, sinceMillis int64, limit int) ([]ViewerMessage, error) {
	evs, err := s.st.PeerGroupMessages(group, sinceMillis, limit)
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]ViewerMessage, 0, len(evs))
	for _, ev := range evs {
		fromName := ev.FromName
		if p := s.peers[ev.FromID]; p != nil && p.Name != "" {
			fromName = p.Name
		}
		toName := ev.ToName
		if p := s.peers[ev.ToID]; p != nil && p.Name != "" {
			toName = p.Name
		}
		out = append(out, ViewerMessage{
			ID: ev.Seq, FromID: ev.FromID, ToID: ev.ToID,
			FromName: fromName, ToName: toName,
			Text: ev.Text, SentAt: isoMillis(ev.SentAt),
		})
	}
	return out, nil
}

// ViewerMessage is one row of the read-only history — the shape today's Mac
// overlay (PeerMessage) and web UI already decode.
type ViewerMessage struct {
	ID       int64  `json:"id"`
	FromID   string `json:"from_id"`
	ToID     string `json:"to_id"`
	FromName string `json:"from_name"`
	ToName   string `json:"to_name"`
	Text     string `json:"text"`
	SentAt   string `json:"sent_at"`
}
