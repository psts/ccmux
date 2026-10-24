package peers

import "log"

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
