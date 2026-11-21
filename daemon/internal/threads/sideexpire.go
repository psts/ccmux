package threads

import (
	"log"

	"ccmux.dev/ccmuxd/internal/store"
)

// ExpireSide ends every side helper whose prompt cache has gone cold and
// that nothing waits on (Cold), unless it still owes a delegator its result
// (Owes). The next question to that repo starts a fresh helper, which costs
// what a cold cache would have cost anyway. A message on its way to a
// helper as it ends is handed back to its sender by the bus (peers
// bounce), never dropped in silence.
//
// Called on a timer (cmd/ccmuxd). A thread that cannot be read is left for
// the next pass.
func (s *Service) ExpireSide() {
	if s.Cold == nil {
		return
	}
	threads, err := s.St.OpenSideThreads()
	if err != nil {
		log.Printf("threads: side helpers not checked for expiry: %v", err)
		return
	}
	for _, th := range threads {
		if s.leadEnded(th) {
			s.closeSide(th) // its main session ended and its helpers were not stood down then
			continue
		}
		s.expireIn(th.ID)
	}
}

// leadEnded says a side thread's main session has ended, or has no row.
// One that cannot be read has not: its helpers wait for the next pass.
func (s *Service) leadEnded(th store.Thread) bool {
	lead, err := s.St.RunnerSession(th.LeadSession)
	if err != nil {
		return false
	}
	return lead == nil || lead.Wanted == store.SessionDone
}

// expireIn ends the cold helpers of one side thread, under its lock.
func (s *Service) expireIn(threadID int64) {
	defer s.lock(threadKey(threadID))()
	helpers, err := s.helpers(threadID)
	if err != nil {
		log.Printf("threads: #%d: side helpers not checked for expiry: %v", threadID, err)
		return
	}
	ended := false
	for _, h := range helpers {
		if !s.expired(h) {
			continue
		}
		left := s.standDown(h)
		log.Printf("threads: #%d: side helper %s ended, its cache had gone cold%s", threadID, h.Name, stayed(left))
		ended = true
	}
	if ended {
		s.reindex()
	}
}

// expired says a side helper may end now.
func (s *Service) expired(h store.RunnerSession) bool {
	if s.Owes != nil && h.PaneID != "" && s.Owes(h.PaneID) {
		return false
	}
	return s.Cold(h.ID)
}
