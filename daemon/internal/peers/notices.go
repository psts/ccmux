package peers

import (
	"errors"
	"log"
)

// NotifyGroup sends text, from the bus itself, to every session present in
// group except those named except (a session never hears about its own
// joining). It reaches sessions on the bus now; one that starts later reads
// the window's members at start instead. Returns how many it reached.
//
// An asleep runner session is present, since a message wakes it, but this
// is not a message to it: like an asleep agent, it is left asleep and reads
// the window's members when it next starts.
func (s *Service) NotifyGroup(group, except, text string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, p := range s.peers {
		if p.Name == except || !s.noticeableLocked(p) || s.groupOfLocked(p) != group {
			continue
		}
		if err := s.deliverLocked(systemEvent(p, group, text)); err != nil {
			log.Printf("peers: window notice to %s in %s not delivered: %v", p.Name, group, err)
			continue
		}
		n++
	}
	return n
}

// noticeableLocked reports a session a group notice should reach: on the
// bus now, and not a runner session that is asleep.
func (s *Service) noticeableLocked(p *Peer) bool {
	return s.presentLocked(p) && !s.asleepRunnerLocked(p)
}

// NotifyPane sends text, from the bus itself, to the session registered on
// paneID: the task runner asking a quiet worker to report. A message wakes
// an idle Claude like any other. Errors when no session is present there,
// so the caller knows nobody heard it.
func (s *Service) NotifyPane(paneID, text string) error {
	return s.NotifyPaneAs(paneID, "", text)
}

// NotifyPaneAs is NotifyPane for a message that has a fixed id (msgID, ""
// for none): the task runner handing a worker back its failed merge, from
// its outbox, which says a notice again when it was cut off before it could
// write down that it was said. A message whose id is in the log already is
// not stored or pushed a second time, and that is no error: it was heard.
func (s *Service) NotifyPaneAs(paneID, msgID, text string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	said, err := s.st.HasPeerMsg(msgID)
	if err != nil || said {
		return err
	}
	for _, p := range s.peers {
		if p.PaneID != paneID || !s.presentLocked(p) {
			continue
		}
		ev := systemEvent(p, s.groupOfLocked(p), text)
		ev.MsgID = msgID
		return s.deliverLocked(ev)
	}
	return errors.New("no Claude is on the bus in that pane")
}
