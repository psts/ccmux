// Send routing, verdict interception, poll, and event delivery.
package peers

import (
	"errors"
	"fmt"
	"log"
	"regexp"
	"strings"
	"time"

	"ccmux.dev/ccmuxd/internal/model"
)

// permReplyRe matches a delegator's verdict reply: "yes abcde" / "no abcde"
// (request ids are five lowercase letters excluding 'l'). Matched centrally at
// route time and ONLY against outstanding requests — an id nobody asked about
// falls through as a normal message instead of silently vanishing.
var permReplyRe = regexp.MustCompile(`(?i)^\s*(y|yes|n|no)\s+([a-km-z]{5})\s*$`)

// answerReplyRe matches a delegator's answer to a relayed question card:
// "answer abcde <text>", the text running to the end (newlines included).
// Same id alphabet and the same outstanding-only rule as permReplyRe.
var answerReplyRe = regexp.MustCompile(`(?is)^\s*answer\s+([a-km-z]{5})\s+(\S.*)$`)

// AnswerText is the answer inside an "answer <id> <text>" reply, trimmed;
// "" when text is not one. The wire and the pane reply both read it from
// the raw message, so an answer needs no column of its own.
func AnswerText(text string) string {
	m := answerReplyRe.FindStringSubmatch(text)
	if m == nil {
		return ""
	}
	return strings.TrimSpace(m[2])
}

// askIDRe is the id alphabet every ask and reply matcher shares: five
// lowercase letters, no l (the sidecar mints ids from the same alphabet).
var askIDRe = regexp.MustCompile(`^[a-km-z]{5}$`)

// ValidAskID reports whether id is one the bus's reply matchers can match.
func ValidAskID(id string) bool { return askIDRe.MatchString(id) }

// PaneReply is what ReplyToPane hands a pane's chat server for the card
// with RequestID: Kind AskPermission with Behavior "allow" or "deny", or
// Kind AskQuestion with Answer the delegator's text.
type PaneReply struct {
	Kind      string
	RequestID string
	Behavior  string
	Answer    string
}

// SendReq mirrors the send_message tool's arguments.
type SendReq struct {
	FromID string `json:"from_id"`
	ToID   string `json:"to_id"`
	ToName string `json:"to_name"`
	// ToGroup addresses a peer in ANOTHER group. Naming the group is the
	// authorization: it turns a cross-project message into a deliberate act
	// rather than something a stale id can do by accident. Empty means the
	// sender's own group, which is the default and the common case.
	ToGroup        string `json:"to_group"`
	Text           string `json:"text"`
	SpawnIfMissing bool   `json:"spawn_if_missing"`
	ToRepo         string `json:"to_repo"`
}

// SendResp is the tool-facing outcome. Error strings the old broker used are
// preserved verbatim — running sessions pattern-match on them.
type SendResp struct {
	OK       bool `json:"ok"`
	Spawning bool `json:"spawning,omitempty"`
	// Queued marks a message accepted into a mailbox with no session attached:
	// durable, but nobody will read it until one returns to that pane. The
	// sender is told, because "sent" and "sent into the void" must not look alike.
	Queued bool   `json:"queued,omitempty"`
	Error  string `json:"error,omitempty"`
}

// Send routes one message: resolve the target in the addressed group, check the
// sender is allowed to reach it, intercept permission verdicts, else append a
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
	if err := s.checkReachableLocked(req, sender, senderGroup, target); err != "" {
		return SendResp{Error: err}
	}

	if handled, resp := s.tryVerdictLocked(sender, target, senderGroup, req.Text); handled {
		return resp
	}
	if handled, resp := s.tryAnswerLocked(sender, target, senderGroup, req.Text); handled {
		return resp
	}
	// A message that crossed a group boundary opens the return path, so the
	// recipient can answer with a plain to_id reply the way it answers anyone
	// else. Without this the conversation is one-way and the loop never closes.
	if targetGroup := s.groupOfLocked(target); !sameGroup(targetGroup, senderGroup) {
		s.grantReplyLocked(target.ID, sender.ID)
	}

	ev := s.eventFromLocked(sender, target, senderGroup, req.Text)
	if err := s.deliverLocked(ev); err != nil {
		return SendResp{Error: err.Error()}
	}
	return SendResp{OK: true, Queued: !s.presentLocked(target)}
}

// resolveTargetLocked finds the addressee: by id, or by name within the
// addressed group — the sender's own unless to_group names another (erroring on
// ambiguity instead of the old silent first-match), optionally spawning a
// missing named teammate.
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
	wantGroup := senderGroup
	if req.ToGroup != "" {
		wantGroup = req.ToGroup
	}
	// By NAME, only present peers are candidates: naming a teammate means "the
	// one that's running", and matching a departed session would silently post
	// into a mailbox instead of falling through to the spawn path. Replies,
	// which carry to_id, still reach an away peer's queue.
	var matches []*Peer
	for _, p := range s.peers {
		if p.Name == req.ToName && p.ID != sender.ID &&
			sameGroup(s.groupOfLocked(p), wantGroup) && s.presentLocked(p) {
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
	claimed, done := s.claimAskLocked(rid, target, AskPermission)
	if !claimed {
		return false, SendResp{} // not an outstanding permission → normal message
	}
	if done {
		return true, SendResp{OK: true}
	}
	behavior := "deny"
	if strings.HasPrefix(strings.ToLower(m[1]), "y") {
		behavior = "allow"
	}
	ev := s.eventFromLocked(sender, target, group, text)
	ev.Kind = model.PeerEventVerdict
	ev.RequestID = rid
	ev.Behavior = behavior
	return true, s.deliverAskReplyLocked(ev, target, PaneReply{Kind: AskPermission, RequestID: rid, Behavior: behavior})
}

// tryAnswerLocked is tryVerdictLocked for "answer <id> <text>": a
// question_answer event for the worker, first answer wins.
func (s *Service) tryAnswerLocked(sender, target *Peer, group, text string) (bool, SendResp) {
	m := answerReplyRe.FindStringSubmatch(text)
	if m == nil {
		return false, SendResp{}
	}
	rid := strings.ToLower(m[1])
	claimed, done := s.claimAskLocked(rid, target, AskQuestion)
	if !claimed {
		return false, SendResp{} // not an outstanding question → normal message
	}
	if done {
		return true, SendResp{OK: true}
	}
	ev := s.eventFromLocked(sender, target, group, text)
	ev.Kind = model.PeerEventAnswer
	ev.RequestID = rid
	return true, s.deliverAskReplyLocked(ev, target, PaneReply{Kind: AskQuestion, RequestID: rid, Answer: AnswerText(text)})
}

// claimAskLocked resolves the outstanding ask rid of kind want for target:
// not claimed when there is none (the reply is a normal message, and a
// permission's id in the answer verb, or a question's in the yes/no verb,
// is exactly that: the ask stays open for the right verb); done when it
// was answered before (first wins, this one is dropped). Persisted before
// delivery: a restart between the two would otherwise let a second reply
// through.
func (s *Service) claimAskLocked(rid string, target *Peer, want string) (claimed, done bool) {
	pr := s.perms[rid]
	if pr == nil || pr.workerID != target.ID || pr.kind != want {
		return false, false
	}
	if pr.resolved {
		return true, true
	}
	s.setAskResolvedLocked(rid, pr, true)
	return true, false
}

// setAskResolvedLocked marks the ask answered (or open again) and writes
// it through, so a restart keeps the same answer.
func (s *Service) setAskResolvedLocked(rid string, pr *permRequest, resolved bool) {
	pr.resolved = resolved
	if err := s.st.SavePermRequest(rid, pr.workerID, pr.kind, resolved, pr.at); err != nil {
		log.Printf("peers: ask %s resolved=%v not persisted, a restart will not keep it: %v", rid, resolved, err)
	}
}

// deliverAskReplyLocked logs and pushes the reply event to the worker, and
// hands it to the worker's pane when the api wired one: an agent behind a
// chat server (the claude sidecar) answers its own card from it. The event
// still reaches its shim, which acks an answer unread and forwards a
// verdict as a permission notification the Claude child has no dialog for.
// The hand-off runs off the lock; when it fails (the chat server did not
// answer) the ask is opened again, so the delegator's next reply is not
// dropped as a duplicate but lands on the card.
func (s *Service) deliverAskReplyLocked(ev *model.PeerEvent, target *Peer, reply PaneReply) SendResp {
	if err := s.deliverLocked(ev); err != nil {
		return SendResp{Error: err.Error()}
	}
	if s.ReplyToPane != nil && target.PaneID != "" {
		go s.replyToPane(target.PaneID, reply)
	}
	return SendResp{OK: true}
}

// replyToPane is deliverAskReplyLocked's hand-off, off the lock.
func (s *Service) replyToPane(paneID string, reply PaneReply) {
	err := s.ReplyToPane(paneID, reply)
	if err == nil || errors.Is(err, ErrNoPanePush) {
		return
	}
	s.mu.Lock()
	if pr := s.perms[reply.RequestID]; pr != nil {
		s.setAskResolvedLocked(reply.RequestID, pr, false)
	}
	s.mu.Unlock()
	log.Printf("peers: reply %s to pane %s did not reach the card, the ask is open again: %v", reply.RequestID, paneID, err)
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
	s.pushToPaneLocked(ev)
	return nil
}

// pushToPaneLocked hands a message to the target's pane when the api wired a
// pane push: an opencode agent instance cannot take channel notifications
// (its shim's push goes to a client that ignores them), so the daemon types
// the message into its TUI through the instance's server. Off the lock and
// best-effort: the mailbox still holds the message for check_messages.
func (s *Service) pushToPaneLocked(ev *model.PeerEvent) {
	if s.PushToPane == nil || ev.Kind != model.PeerEventMessage {
		return
	}
	target := s.peers[ev.ToID]
	if target == nil || target.PaneID == "" {
		return
	}
	paneID, text := target.PaneID, pushText(ev)
	go func() {
		if err := s.pushOrWake(paneID, text); err != nil && !errors.Is(err, ErrNoPanePush) {
			log.Printf("peers: pane push to %s skipped: %v", paneID, err)
		}
	}()
}

// pushRetryEvery and pushRetries bound the wait for a harness that another
// start is bringing up: the push is retried until the TUI answers.
const (
	pushRetryEvery = 2 * time.Second
	pushRetries    = 5
)

// pushOrWake delivers text to a pane: push into a running TUI; wake an asleep
// agent with it as first prompt; and when another start is already in
// flight (whose prompt is not this message), retry the push until the
// harness is up and takes it.
func (s *Service) pushOrWake(paneID, text string) error {
	err := s.PushToPane(paneID, text)
	if errors.Is(err, ErrAgentAsleep) && s.WakePane != nil {
		err = s.WakePane(paneID, text)
	}
	if !errors.Is(err, ErrStartInFlight) {
		return err
	}
	// The pane still reads asleep while the other start's harness comes up;
	// keep trying until the TUI takes the push or the wait runs out.
	for i := 0; i < pushRetries; i++ {
		time.Sleep(pushRetryEvery)
		if err = s.PushToPane(paneID, text); !errors.Is(err, ErrAgentAsleep) {
			return err
		}
	}
	return err
}

// ErrNoPanePush is what a PushToPane hook returns for a pane that takes no
// TUI push — normal peer traffic — so the bus does not log every message.
// ErrAgentAsleep says the pane is an agent at its shell: WakePane instead.
// ErrStartInFlight from WakePane says another start of that agent is under
// way with a different first prompt; pushOrWake retries the push.
var (
	ErrNoPanePush    = errors.New("pane takes no push")
	ErrAgentAsleep   = errors.New("agent pane is asleep")
	ErrStartInFlight = errors.New("agent start in flight")
)

// pushText renders a bus message the way a channel tag would, so an agent
// reading it as a user turn still knows who wrote it and how to answer.
func pushText(ev *model.PeerEvent) string {
	return fmt.Sprintf("[claude-peers message from %s (id %s)]\n%s\n\nReply with send_message to_id=%s.",
		ev.FromName, ev.FromID, ev.Text, ev.FromID)
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
	s.touchLocked(peerID)
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
