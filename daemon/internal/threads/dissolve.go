package threads

import (
	"errors"
	"fmt"
	"log"

	"ccmux.dev/ccmuxd/internal/jobs"
	"ccmux.dev/ccmuxd/internal/store"
)

// dissolve takes a landed thread's team apart: the helpers are stood down,
// the lead goes back to its repo's own folder, reading, and its worktree
// is removed once it is out of it. It returns what had to stay.
//
// It can be cut off anywhere (the daemon restarts) and is then run again
// from the top, until it has once run to its end (its caller records
// that), so it is built to be repeated:
//   - It works from the team as the merge found it (ask), kept on the
//     job's row, and not only from the rows as they are now. A helper whose
//     row already says done, or a lead whose row no longer names a
//     worktree, still has its worktree removed.
//   - A worktree is removed before the row that names it is cleared, so a
//     worktree never stands with nothing pointing at it.
//   - Each step is written down in steps once it is done, with what it
//     left, and skipped the second time. That matters for the lead's
//     restart, which would otherwise cut off a turn it had started since.
func (s *Service) dissolve(ask mergeAsk, steps *jobs.Journal) []string {
	left := s.endHelpers(ask, steps)
	lead := ask.lead()
	if lead.Worktree == "" {
		return left
	}
	return append(left, s.leadHome(lead, steps)...)
}

// endHelpers stands down every helper a merge's end takes apart, each once,
// and returns what stayed.
func (s *Service) endHelpers(ask mergeAsk, steps *jobs.Journal) []string {
	helpers, left := s.helpersToEnd(ask)
	for _, h := range helpers {
		left = append(left, once(steps, "stood-down:"+h.ID, func() []string { return s.standDown(h) })...)
	}
	return left
}

// once does a step unless steps says it is done, and returns what it left
// either way. A record that cannot be written is logged: the step is then
// done again next time, which every step here allows.
func once(steps *jobs.Journal, key string, step func() []string) []string {
	var left []string
	if steps.Done(key, &left) {
		return left
	}
	left = step()
	if err := steps.Record(key, left); err != nil {
		log.Printf("threads: step %s was done and not written down, so it may be done again: %v", key, err)
	}
	return left
}

// helpersToEnd is every helper a landed thread stands down: the ones the
// merge found, as it found them, and any that joined since. A helper's row
// as it is now wins over the merge's copy (it may have got its worktree
// since), unless it has ended: then the copy is all that still names its
// worktree. When the rows cannot be read, the merge's copy stands alone and
// the second return says so.
func (s *Service) helpersToEnd(ask mergeAsk) (helpers []store.RunnerSession, left []string) {
	seen := map[string]int{}
	for _, m := range ask.sessions() {
		if m.Role != store.RoleLead {
			seen[m.ID] = len(helpers)
			helpers = append(helpers, m)
		}
	}
	now, err := s.helpers(ask.ThreadID)
	if err != nil {
		return helpers, []string{fmt.Sprintf("the helpers that joined during the merge (their rows could not be read: %v)", err)}
	}
	for _, m := range now {
		if i, had := seen[m.ID]; had {
			helpers[i] = m
			continue
		}
		helpers = append(helpers, m)
	}
	return helpers, nil
}

// leadHome moves a landed thread's lead back to its repo's own folder,
// reading, and removes its worktree. In this order: the lead leaves (its
// row still names the worktree), the worktree goes, and only then does the
// row forget it. A lead that could not leave keeps its worktree, and its
// row keeps saying so.
func (s *Service) leadHome(lead store.RunnerSession, steps *jobs.Journal) []string {
	stuck := once(steps, "lead-out", func() []string {
		out := store.SessionPlace{Cwd: lead.Repo, Worktree: lead.Worktree, Branch: lead.Branch, ReadOnly: true}
		if err := s.Sessions.Place(lead.ID, out); err != nil {
			return []string{fmt.Sprintf("%s (the lead could not leave it: %v)", lead.Worktree, err)}
		}
		if err := s.Sessions.Relocate(lead.ID); err != nil {
			return []string{fmt.Sprintf("%s (the lead was not restarted out of it: %v)", lead.Worktree, err)}
		}
		return nil
	})
	if len(stuck) > 0 {
		return stuck
	}
	left := once(steps, "lead-worktree", func() []string { return s.removeWorktree(lead) })
	return append(left, once(steps, "lead-row", func() []string {
		if err := s.Sessions.Place(lead.ID, store.SessionPlace{Cwd: lead.Repo, ReadOnly: true}); err != nil {
			return []string{fmt.Sprintf("the lead's record still names %s (%v)", lead.Worktree, err)}
		}
		return nil
	})...)
}

// gone reports whether stopping a session failed only because it is not
// there to stop: its row is gone. Its worktree is then still to remove.
func gone(err error) bool { return errors.Is(err, store.ErrNoSession) }
