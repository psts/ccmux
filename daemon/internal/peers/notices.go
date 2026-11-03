package peers

import (
	"errors"
	"log"
)

// NotifyGroup sends text, from the bus itself, to every session present in
// group except those named except (a session never hears about its own
// joining). It reaches sessions on the bus now; one that starts later reads
// the window's members at start instead. Returns how many it reached.
func (s *Service) NotifyGroup(group, except, text string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, p := range s.peers {
		if p.Name == except || !s.presentLocked(p) || s.groupOfLocked(p) != group {
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

// NotifyPane sends text, from the bus itself, to the session registered on
// paneID: the task runner handing a worker back its failed merge. A message
// wakes an idle Claude like any other. Errors when no session is present
// there, so the caller knows nobody heard it.
func (s *Service) NotifyPane(paneID, text string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, p := range s.peers {
		if p.PaneID != paneID || !s.presentLocked(p) {
			continue
		}
		return s.deliverLocked(systemEvent(p, s.groupOfLocked(p), text))
	}
	return errors.New("no Claude is on the bus in that pane")
}
