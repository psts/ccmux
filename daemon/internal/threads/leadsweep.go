package threads

import (
	"log"

	"ccmux.dev/ccmuxd/internal/store"
)

// SweepLeadless takes apart every thread whose lead has ended and that is
// still open: the lead's end was heard and could not finish (a row or the
// jobs could not be read then, or the daemon stopped in between), and
// nothing else would ever stand its helpers down. A merging thread is left
// to its merge's end, which takes it apart (ending.leadless).
//
// Called on a timer (cmd/ccmuxd). What cannot be read is left for the next
// pass.
func (s *Service) SweepLeadless() {
	list, err := s.St.LeadlessThreads()
	if err != nil {
		log.Printf("threads: threads whose lead ended not checked: %v", err)
		return
	}
	for _, th := range list {
		lead, err := s.St.RunnerSession(th.LeadSession)
		if err != nil || lead == nil || lead.Wanted != store.SessionDone || lead.ThreadID != th.ID {
			continue
		}
		log.Printf("threads: #%d lost its lead and was not taken apart then; taking it apart now", th.ID)
		s.leadEndedThread(*lead)
	}
}
