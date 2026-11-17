package threads

import (
	"fmt"
	"log"
	"path/filepath"
	"strings"

	"ccmux.dev/ccmuxd/internal/model"
	"ccmux.dev/ccmuxd/internal/store"
	"ccmux.dev/ccmuxd/internal/taskq"
)

// Merge lands a thread: every member's branch, the lead's own included,
// through the task queue's merge (taskq.Lander: each is rebased onto its
// repo's trunk, the repo's checks run in its worktree, and all trunks move
// forward or none does). The checks take minutes, so this only starts it
// and says so; how it went reaches the lead as a message (land).
//
// It is refused, with the reason, while a helper is still mid-turn or waits
// on a card (its work is not all there yet), while a repo's own folder
// cannot take the merge, and when no member changed anything.
func (s *Service) Merge(threadID int64, by string) (string, error) {
	defer s.lock(threadKey(threadID))()
	th, lead, err := s.led(threadID)
	if err != nil {
		return "", err
	}
	if th.State == store.ThreadMerging {
		return "", fmt.Errorf("thread #%d is being merged already; its result comes as a message", threadID)
	}
	members, err := s.St.ThreadMembers(threadID)
	if err != nil {
		return "", err
	}
	work := worked(members)
	if why := mergeRefusal(th, members, work); why != "" {
		return "", fmt.Errorf("%s", why)
	}
	if why := s.Lander.Blocked(works(work)); why != "" {
		return "", fmt.Errorf("thread #%d cannot merge yet: %s", threadID, strings.TrimPrefix(strings.TrimPrefix(why, "waiting to merge: "), "waiting: "))
	}
	th.State, th.Note, th.UpdatedAt = store.ThreadMerging, "", s.millis()
	if err := s.St.SaveThread(th); err != nil {
		return "", err
	}
	go s.land(th, lead, work, by)
	return fmt.Sprintf("Merging thread #%d: %s. Each repo's checks run now, which takes minutes. "+
		"The result comes to the lead as a message; change nothing in a worktree meanwhile.", threadID, repoList(work)), nil
}

// worked is the members that have a worktree: the ones with a branch to land.
func worked(members []store.RunnerSession) []store.RunnerSession {
	var out []store.RunnerSession
	for _, m := range members {
		if m.Worktree != "" {
			out = append(out, m)
		}
	}
	return out
}

func works(members []store.RunnerSession) []taskq.Work {
	out := make([]taskq.Work, len(members))
	for i, m := range members {
		out[i] = workOf(m)
	}
	return out
}

func repoList(members []store.RunnerSession) string {
	list := make([]string, len(members))
	for i, m := range members {
		list[i] = filepath.Base(m.Repo)
	}
	return strings.Join(list, ", ")
}

// mergeRefusal says why a thread's merge does not start, "" when it does.
func mergeRefusal(th store.Thread, members, work []store.RunnerSession) string {
	if len(work) == 0 {
		return fmt.Sprintf("thread #%d has nothing to merge: no member has changed anything", th.ID)
	}
	for _, m := range members {
		if m.Role == store.RoleLead {
			continue // the lead is mid-turn by asking, and a person merges over its question
		}
		switch model.Attention(m.Attention) {
		case model.AttentionRunning:
			return fmt.Sprintf("%s is still working; wait for it to report, then merge", m.Name)
		case model.AttentionNeedsInput:
			return fmt.Sprintf("%s is waiting on an answer (a question or an approval): a merge would stand it down with its card open. Answer it, or stand it down, then merge", m.Name)
		}
	}
	return ""
}

// land runs a thread's merge and settles it. Failed: the thread is open
// again, nothing of the team is touched, and the lead is told why, with the
// member whose worktree the failure is in when it is one member's to fix.
// Landed: the helpers are stood down, every worktree goes (the lead is back
// in its repo's folder first, reading), and the lead is told.
func (s *Service) land(th store.Thread, lead store.RunnerSession, work []store.RunnerSession, by string) {
	note, err := s.Lander.Land(works(work))
	defer s.lock(threadKey(th.ID))()
	if err != nil {
		log.Printf("threads: #%d merge failed: %v", th.ID, err)
		s.settle(th, store.ThreadOpen, "merge failed: "+firstLine(err.Error()))
		s.tell(lead, asked(by)+failedText(th, work, err))
		return
	}
	log.Printf("threads: #%d %s", th.ID, note)
	left := s.dissolve(th, lead)
	s.settle(th, store.ThreadMerged, note+stayed(left))
	s.reindex()
	s.tell(lead, asked(by)+landedText(th, lead, note, left))
}

// asked opens the word to the lead on a merge it did not ask for itself.
func asked(by string) string {
	if by == ByPerson {
		return "[ccmux] A person asked for this thread's merge on its page.\n\n"
	}
	return ""
}

// settle records how a merge ended.
func (s *Service) settle(th store.Thread, state, note string) {
	th.State, th.Note, th.UpdatedAt = state, note, s.millis()
	if err := s.St.SaveThread(th); err != nil {
		log.Printf("threads: #%d: its merge's end (%s) was not recorded: %v", th.ID, state, err)
	}
}

// tell sends a thread's lead a notice as a prompt, which wakes it.
func (s *Service) tell(lead store.RunnerSession, text string) {
	if _, err := s.Sessions.Send(lead.ID, text, sender); err != nil {
		log.Printf("threads: #%d: its lead was not told (%v): %s", lead.ThreadID, err, firstLine(text))
	}
}

func firstLine(text string) string {
	line, _, _ := strings.Cut(text, "\n")
	return line
}

// dissolve takes a landed thread's team apart: the helpers are stood down,
// the lead goes back to its repo's own folder, reading, and its worktree
// is removed once it is out of it. It returns what had to stay.
func (s *Service) dissolve(th store.Thread, lead store.RunnerSession) []string {
	left, err := s.standAllDown(th.ID)
	if err != nil {
		left = append(left, fmt.Sprintf("the helpers (their rows could not be read: %v)", err))
	}
	if lead.Worktree == "" {
		return left
	}
	if err := s.Sessions.Place(lead.ID, store.SessionPlace{Cwd: lead.Repo, ReadOnly: true}); err != nil {
		return append(left, fmt.Sprintf("%s (the lead could not leave it: %v)", lead.Worktree, err))
	}
	if err := s.Sessions.Relocate(lead.ID); err != nil {
		return append(left, fmt.Sprintf("%s (the lead was not restarted out of it: %v)", lead.Worktree, err))
	}
	if why := s.Lander.Remove(workOf(lead)); why != "" {
		left = append(left, why)
	}
	return left
}

// failedText is what the lead reads of a merge that failed.
func failedText(th store.Thread, work []store.RunnerSession, err error) string {
	whose := "Fix it, then ask for the merge again."
	if i, ok := taskq.Fixable(err); ok && i < len(work) {
		m := work[i]
		whose = fmt.Sprintf("This is in %s's worktree (%s). ", m.Name, m.Worktree)
		if m.Role == store.RoleLead {
			whose += "That is yours: fix it there, commit, then ask for the merge again."
		} else {
			whose += "Pass it to that helper, wait for it to report, then ask for the merge again."
		}
	}
	return fmt.Sprintf("[ccmux] Thread #%d: the merge failed, and the thread stays open with its team as it was.\n\n%s\n\n"+
		"A failed check was run twice before it came to you; where a file is named above it holds that check's whole output. %s",
		th.ID, err.Error(), whose)
}

// landedText is what the lead reads of a merge that landed.
func landedText(th store.Thread, lead store.RunnerSession, note string, left []string) string {
	where := ""
	if lead.Worktree != "" {
		where = fmt.Sprintf(" You are back in %s, the repo's own folder, where you only read; a new change gets a fresh worktree.", lead.Repo)
	}
	return fmt.Sprintf("[ccmux] Thread #%d: %s. Nothing was pushed. Every helper is stood down and the worktrees are gone%s.%s "+
		"Tell the person what landed.", th.ID, note, stayed(left), where)
}

// Recover puts a thread the daemon was merging when it stopped back to
// open and tells its lead: the merge is asked for again, and what already
// landed is skipped by it. Called once at start, with the member index.
func (s *Service) Recover() {
	s.reindex()
	threads, err := s.St.Threads()
	if err != nil {
		log.Printf("threads: not read at start, a merge the daemon dropped stays marked as merging: %v", err)
		return
	}
	for _, th := range threads {
		if th.State != store.ThreadMerging {
			continue
		}
		s.settle(th, store.ThreadOpen, "its merge was cut off by a daemon restart")
		if lead, err := s.St.RunnerSession(th.LeadSession); err == nil && lead != nil {
			s.tell(*lead, fmt.Sprintf("[ccmux] Thread #%d: its merge was cut off by a daemon restart, so it may be half done. "+
				"Ask for the merge again: what already landed is skipped.", th.ID))
		}
	}
}
