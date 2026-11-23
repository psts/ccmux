package threads

import (
	"errors"
	"fmt"
	"log"

	"ccmux.dev/ccmuxd/internal/jobs"
	"ccmux.dev/ccmuxd/internal/store"
	"ccmux.dev/ccmuxd/internal/taskq"
)

// A thread's lead that ends while the thread is being merged does not take
// the thread apart at once: the merge job is in the team's worktrees, and
// removing them under it would fail its checks halfway, or worse. The job
// is stopped instead, and when its end is settled (mergeHandler), the
// thread is taken apart as SessionEnded would have done, and reads as
// ended with what the merge had landed. Nobody is told: there is no lead to
// tell. The log and the thread's note say it.

// stoppedLeadEnded is the stop's note on the job's row.
const stoppedLeadEnded = "stopped because the thread's lead ended"

// mergeStop is what leadEndedMerge found of a thread's merge.
type mergeStop int

const (
	noMerge      mergeStop = iota // nothing runs in the team's worktrees: take the thread apart now
	mergeRunning                  // a job is (or may be) in them: its end takes the thread apart
	mergeEnded                    // the job ended as it was being stopped: its end has been settled
)

// leadEndedThread takes apart the thread of a lead that ended, unless its merge
// runs: then the merge is stopped, and its end does it (ending.leadless).
func (s *Service) leadEndedThread(sess store.RunnerSession) {
	merge := s.leadEndedMerge(sess.ThreadID)
	if merge == mergeRunning {
		return
	}
	defer s.lock(threadKey(sess.ThreadID))()
	th, err := s.St.Thread(sess.ThreadID)
	if err != nil || th == nil {
		return
	}
	if merge == mergeEnded && th.State == store.ThreadEnded {
		return // the merge's end saw the lead gone and took the thread apart
	}
	_ = s.takeApart(sess, *th) // logged there; nobody waits on a session's end
}

// leadEndedMerge stops the merge of a thread whose lead ended, if one runs.
// The thread's row and its open job are read under the thread's lock (a
// merge is started under it, so "merging" with no job yet is never seen),
// and the job is stopped off it: a job's end is settled under the job's
// lock and then the thread's (jobs.Manager.lock), so asking for a stop
// while holding the thread's would turn that order around.
func (s *Service) leadEndedMerge(threadID int64) mergeStop {
	job, merge := s.openMerge(threadID)
	if job == nil {
		return merge
	}
	if job.State == store.JobStopping {
		log.Printf("threads: #%d lost its lead while its merge was being stopped; the merge's end takes the thread apart", threadID)
		return mergeRunning
	}
	err := s.Jobs.Stop(job.ID, stoppedLeadEnded)
	switch {
	case errors.Is(err, store.ErrNoJob):
		return mergeEnded
	case err != nil:
		// Asked and not heard: the job runs to its end, and that end takes
		// the thread apart all the same.
		log.Printf("threads: #%d lost its lead during its merge, which was not stopped (%v); its end takes the thread apart", threadID, err)
	default:
		log.Printf("threads: #%d lost its lead during its merge: stopping %s; its end takes the thread apart", threadID, job.ID)
	}
	return mergeRunning
}

// openMerge is a merging thread's open job. With no job the second return
// says what to do: noMerge (the thread is not merging, or it is with no
// job to stop) or mergeRunning (the jobs could not be read, which never
// counts as "no job": nothing is removed under a merge that may run).
func (s *Service) openMerge(threadID int64) (*store.Job, mergeStop) {
	defer s.lock(threadKey(threadID))()
	th, err := s.St.Thread(threadID)
	if err != nil || th == nil || th.State != store.ThreadMerging {
		return nil, noMerge
	}
	if s.Jobs == nil {
		log.Printf("threads: #%d lost its lead while it reads as merging, and this daemon runs no jobs; taking it apart", threadID)
		return nil, noMerge
	}
	job, err := s.Jobs.OpenJob(mergeSubject(threadID))
	if err != nil {
		log.Printf("threads: #%d LOST ITS LEAD DURING ITS MERGE and its merge job was not read (%v); its team and worktrees are left as they are", threadID, err)
		return nil, mergeRunning
	}
	if job == nil {
		log.Printf("threads: #%d lost its lead while it reads as merging with no merge job; taking it apart", threadID)
	}
	return job, noMerge
}

// takeApart stands a lead-less thread's helpers down, removes the lead's
// worktree when nothing in it would be lost, and marks the thread ended. A
// thread that reads as ended already keeps its note, with what stayed
// added: the merge's end may have written what landed there. What failed
// is logged and returned.
func (s *Service) takeApart(sess store.RunnerSession, th store.Thread) error {
	left, err := s.standAllDown(sess.ThreadID)
	if err != nil {
		log.Printf("threads: #%d lost its lead and its helpers were not stood down: %v", sess.ThreadID, err)
		return err
	}
	if sess.Worktree != "" {
		if why := s.Lander.Remove(workOf(sess), taskq.RebaseMark(sess.Worktree)); why != "" {
			left = append(left, why)
		}
	}
	note := stayedNote(left)
	if th.State == store.ThreadEnded && th.Note != "" {
		note = th.Note + stayed(left)
	}
	th.State, th.Note, th.UpdatedAt = store.ThreadEnded, note, s.millis()
	if err := s.St.SaveThread(th); err != nil {
		log.Printf("threads: #%d ended with its lead but is not marked so: %v", th.ID, err)
		return err
	}
	return nil
}

// leadGone reports whether a thread's lead has ended: its row says done,
// or it is gone. A row that cannot be read is an error, and the job's end
// is settled again later.
func (s *Service) leadGone(id string) (bool, error) {
	lead, err := s.St.RunnerSession(id)
	if err != nil {
		return false, fmt.Errorf("its lead's row was not read: %w", err)
	}
	return lead == nil || lead.Wanted == store.SessionDone, nil
}

// leadless settles a merge whose thread's lead has ended: what SessionEnded
// left to the merge's end is done now. The helpers are stood down, the
// lead's worktree goes (the lead is not moved out of it first: it has
// ended), and the thread reads as ended with note, the merge's own word on
// what landed. Each step is in the applied journal, so a second settling
// does nothing more.
func (e *ending) leadless(note string) error {
	left := once(e.steps, "dissolved", func() []string { return e.s.dissolveLeadless(e.ask, e.steps) })
	note = "its lead ended during the merge; " + note + stayed(left)
	if err := e.settle(store.ThreadEnded, note); err != nil {
		return err
	}
	log.Printf("threads: #%d ended with its lead: %s", e.ask.ThreadID, note)
	e.s.reindex()
	e.s.changed(e.ask.lead().ID)
	return nil
}

// dissolveLeadless is dissolve for a thread whose lead has ended: the
// helpers go as ever, and the lead's worktree is removed where it stands.
func (s *Service) dissolveLeadless(ask mergeAsk, steps *jobs.Journal) []string {
	left := s.endHelpers(ask, steps)
	lead := ask.lead()
	if lead.Worktree == "" {
		return left
	}
	return append(left, once(steps, "lead-worktree", func() []string { return s.removeWorktree(lead) })...)
}

// removeWorktree removes a member's worktree and branch, forcing nothing,
// and returns what stayed.
func (s *Service) removeWorktree(m store.RunnerSession) []string {
	if why := s.Lander.Remove(workOf(m), taskq.RebaseMark(m.Worktree)); why != "" {
		return []string{why}
	}
	return nil
}

// orphanedLeadless takes apart the thread of an unreadable merge whose lead
// has ended, from the rows as they are (what the merge kept of the team may
// be what could not be read). It forces nothing, so a worktree whose work
// may not have landed stays, and every step of it may be done again: a
// thread that was not saved as ended is an error, and the job is settled
// again.
func (s *Service) orphanedLeadless(th store.Thread, note string) error {
	lead, err := s.St.RunnerSession(th.LeadSession)
	if err != nil {
		return err
	}
	if lead == nil {
		lead = &store.RunnerSession{ThreadID: th.ID}
	}
	th.State, th.Note = store.ThreadEnded, "its lead ended during the merge; "+note
	log.Printf("threads: #%d ended with its lead: %s", th.ID, th.Note)
	defer s.reindex()
	if err := s.takeApart(*lead, th); err != nil {
		return fmt.Errorf("thread #%d: its lead ended and it was not taken apart: %w", th.ID, err)
	}
	return nil
}

// orphanedEnd is orphaned for a thread whose lead has ended, or may have:
// a lead whose row was not read is an error, and the job is settled again.
func (s *Service) orphanedEnd(th store.Thread, note string, err error) error {
	if err != nil {
		return fmt.Errorf("thread #%d: %w", th.ID, err)
	}
	return s.orphanedLeadless(th, note)
}
