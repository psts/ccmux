// Runner sessions on the bus. A runner session (internal/runner) is a Claude
// session on the Agent SDK with no terminal: its pane (manager/runnerpanes.go)
// gives it the same derived peer id, token and window group a tmux pane's
// session has, and three things differ.
//
// The daemon registers its peer (EnsureRunnerPeer), and names it. A peer made
// only by its shim would vanish at every daemon restart until the shim came
// back, and an asleep session has no shim at all, while a message is exactly
// what wakes it.
//
// It is present for as long as its session has not ended: asleep or running,
// a message reaches it, so a listing that shows it tells the truth.
//
// Its messages arrive as prompts. Claude Code does not hand a channel push to
// the model of an SDK session, so each message is taken from the peer's
// mailbox, in order, and queued in the session's inbox (RunnerInbox), which
// wakes an asleep session and delivers once. The cursor moves past a message
// only when the inbox has it, and check_messages hands a runner nothing from
// the mailbox, so no message is ever shown twice.
package peers

import (
	"errors"
	"fmt"
	"log"
	"sync"

	"github.com/google/uuid"

	"ccmux.dev/ccmuxd/internal/model"
)

// ErrInboxClosed is what RunnerInbox returns for a session that has ended:
// nothing will ever read the message, so the drain moves past it.
var ErrInboxClosed = errors.New("the session has ended")

// RunnerInfo is what the daemon says of a runner session's pane: its name
// on the bus, its workspace's repo folder, and whether it is asleep.
type RunnerInfo struct {
	Name, Repo string
	Asleep     bool
}

// runnerLocked reports whether paneID is a runner session's pane, and what
// the daemon says of it. With no inbox to hand messages to (session runners
// are off on this host) no pane is one: a runner peer would be kept present
// and take messages that nothing ever delivers.
func (s *Service) runnerLocked(paneID string) (RunnerInfo, bool) {
	if s.RunnerPane == nil || s.RunnerInbox == nil || paneID == "" {
		return RunnerInfo{}, false
	}
	return s.RunnerPane(paneID)
}

func (s *Service) runnerPeerLocked(p *Peer) bool {
	_, ok := s.runnerLocked(p.PaneID)
	return ok
}

// asleepRunnerLocked reports a runner session that is asleep. It is present
// (a message wakes it), so whatever is sent to everyone present must ask
// this first: a notice meant for nobody in particular is no reason to start
// a session and spend a turn on it.
func (s *Service) asleepRunnerLocked(p *Peer) bool {
	info, ok := s.runnerLocked(p.PaneID)
	return ok && info.Asleep
}

// stampRunnerLocked makes a peer on a runner's pane say what the daemon says
// of it, whatever its shim sent: the session's bus name, and its workspace's
// repo as its git root, so in a workspace outside any window it falls back
// to the same group as the repo's other sessions. Any other peer is left as
// it registered.
func (s *Service) stampRunnerLocked(p *Peer) {
	info, ok := s.runnerLocked(p.PaneID)
	if !ok {
		return
	}
	p.Name = info.Name
	if info.Repo != "" {
		p.GitRoot = info.Repo
	}
}

// EnsureRunnerPeer registers the peer of a runner session's pane, or renames
// and moves the one already there, and hands over whatever waits in its
// mailbox. The
// daemon calls it when a session is made or woken, and for every session at
// its own start. A pane that is not a runner's is left alone.
func (s *Service) EnsureRunnerPeer(paneID, cwd string) {
	s.mu.Lock()
	if _, ok := s.runnerLocked(paneID); !ok {
		s.mu.Unlock()
		return
	}
	id := derivedID(paneID)
	p := s.peers[id]
	if p == nil {
		now := s.Now().UnixMilli()
		p = &Peer{ID: id, PaneID: paneID, CWD: cwd, RegisteredAt: now, LastSeenAt: now, PollOnly: true}
		s.peers[id] = p
		_ = s.st.TouchPeerMailbox(id, substrateKey(p), now)
	}
	if p.PaneID != paneID {
		s.mu.Unlock()
		log.Printf("peers: runner pane %s shares its derived id %s with pane %s; it stays off the bus", paneID, id, p.PaneID)
		return
	}
	p.CWD = cwd // a thread member moves into its worktree, and back
	s.stampRunnerLocked(p)
	// A helper a thread asked for by name is on the bus from here on: what
	// was sent to start it is delivered now (helperroute.go).
	s.fulfillPendingSpawnLocked(p)
	s.mu.Unlock()
	go s.drainInbox(id, paneID)
}

// ForgetRunnerPeer drops the peer of a runner session that has ended, and
// its mailbox: nothing will read it again, and until the sweep noticed the
// pane gone it would still be listed, taking messages nobody answers.
func (s *Service) ForgetRunnerPeer(paneID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	id := derivedID(paneID)
	if p := s.peers[id]; p != nil && p.PaneID == paneID {
		s.dropPeerLocked(id)
	}
}

// inboxLocks serializes drains per peer: two messages arriving together must
// reach the inbox in mailbox order, and one session's slow inbox must not
// hold up another's.
var inboxLocks sync.Map // peer id → *sync.Mutex

func inboxLock(peerID string) *sync.Mutex {
	mu, _ := inboxLocks.LoadOrStore(peerID, &sync.Mutex{})
	return mu.(*sync.Mutex)
}

// drainInbox hands a runner peer's undelivered mailbox to its session's
// inbox, oldest first, moving the cursor past each event once it is handed
// over. An inbox that refuses stops the drain where it is: the next message,
// the next sweep (ReapOnce) or the next daemon start goes on from there.
func (s *Service) drainInbox(peerID, paneID string) {
	if s.RunnerInbox == nil {
		return
	}
	mu := inboxLock(peerID)
	mu.Lock()
	defer mu.Unlock()
	evs, err := s.undrained(peerID)
	if err != nil {
		log.Printf("peers: runner peer %s: mailbox unreadable, its messages wait: %v", peerID, err)
		return
	}
	for _, ev := range evs {
		if !s.handToInbox(paneID, peerID, ev) {
			return
		}
		s.mu.Lock()
		err := s.st.AdvancePeerCursor(peerID, ev.Seq)
		s.mu.Unlock()
		if err != nil {
			// The prompt is queued under the message's own id, so handing it
			// over again after this queues nothing.
			log.Printf("peers: runner peer %s: cursor not moved past %d: %v", peerID, ev.Seq, err)
			return
		}
	}
}

func (s *Service) undrained(peerID string) ([]*model.PeerEvent, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	cursor, err := s.st.PeerCursor(peerID)
	if err != nil {
		return nil, err
	}
	return s.st.PeerEventsAfter(peerID, cursor)
}

// handToInbox queues one mailbox event as a prompt and reports whether the
// drain may move past it. Only messages become prompts: a verdict or an
// answer goes to the card it is for (ReplyToPane), and the event is in the
// mailbox only so the log keeps it.
func (s *Service) handToInbox(paneID, peerID string, ev *model.PeerEvent) bool {
	if ev.Kind != model.PeerEventMessage {
		return true
	}
	err := s.RunnerInbox(paneID, InboxPromptID(peerID, ev.Seq), pushText(ev), ev.FromName)
	switch {
	case err == nil:
		return true
	case errors.Is(err, ErrInboxClosed):
		log.Printf("peers: runner peer %s: message %d from %s dropped, its session has ended", peerID, ev.Seq, ev.FromName)
		return true
	}
	log.Printf("peers: runner peer %s: message %d from %s not handed to its session yet: %v", peerID, ev.Seq, ev.FromName, err)
	return false
}

// inboxSpace is the namespace prompt ids are derived in.
var inboxSpace = uuid.MustParse("6f2d7c1e-8a0b-4e39-9c55-1d0f6b3a2c71")

// InboxPromptID names the prompt a mailbox event becomes: the same event
// always gets the same id, so a hand-over repeated after a crash between the
// inbox write and the cursor write queues nothing new. A UUID, because it is
// the prompt's id in Claude's transcript.
func InboxPromptID(peerID string, seq int64) string {
	return uuid.NewSHA1(inboxSpace, []byte(fmt.Sprintf("%s:%d", peerID, seq))).String()
}

// drainRunnersLocked starts a drain for every runner peer: the sweep's
// retry for a hand-over that failed.
func (s *Service) drainRunnersLocked() {
	if s.RunnerInbox == nil {
		return
	}
	for id, p := range s.peers {
		if s.runnerPeerLocked(p) {
			go s.drainInbox(id, p.PaneID)
		}
	}
}
