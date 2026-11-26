package peers

import (
	"errors"
	"fmt"
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
	return s.NotifyGroupSkip(group, except, nil, text)
}

// NotifyGroupSkip is NotifyGroup that also leaves out every session whose
// pane skip names: a repo's own sessions go by more names than its own (a
// runner session registers as <repo>-s-<id>), so a name alone does not keep
// a joining repo from hearing about itself. skip runs under the bus lock:
// it may read the manager's memory, never the store.
func (s *Service) NotifyGroupSkip(group, except string, skip func(paneID string) bool, text string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, p := range s.peers {
		if p.Name == except || (skip != nil && skip(p.PaneID)) || !s.noticeableLocked(p) || s.groupOfLocked(p) != group {
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

// NotifyNamed sends text, from the bus itself, to whoever name reaches in
// group, as send_message(to_name=name) would: a present peer called exactly
// that, else the main session of the repo whose folder is called that
// (mainsession.go; asleep counts, and the message wakes it). The task
// runner telling a task's filer that it merged. quiet stores a notice that
// wakes nobody instead (quiet.go), and a quiet one may wait for an away
// peer. It never starts a session. msgID is the message's fixed id, as for
// NotifyPaneAs: said again, it is not stored a second time.
func (s *Service) NotifyNamed(group, name, msgID, text string, quiet bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	said, err := s.st.HasPeerMsg(msgID)
	if err != nil || said {
		return err
	}
	if quiet {
		return s.notifyQuietLocked(group, name, msgID, text)
	}
	matches, _ := s.answeringLocked(name, group, "")
	if len(matches) != 1 {
		return fmt.Errorf("%d sessions answer to %q in %q, want one", len(matches), name, group)
	}
	ev := systemEvent(matches[0], group, text)
	ev.MsgID = msgID
	return s.deliverLocked(ev)
}

// notifyQuietLocked stores a quiet notice from the bus for name in group.
func (s *Service) notifyQuietLocked(group, name, msgID, text string) error {
	target := s.quietNamedLocked(name, "", group)
	if target == nil {
		return fmt.Errorf("no session answers to %q in %q", name, group)
	}
	ev := systemEvent(target, group, text)
	ev.MsgID, ev.Quiet, ev.SentAt = msgID, true, s.Now().UnixMilli()
	if _, err := s.st.AppendPeerEvent(ev); err != nil {
		return err
	}
	s.fanToListenersLocked(ev)
	return nil
}
