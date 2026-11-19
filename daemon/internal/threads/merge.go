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
// forward or none does). The checks take minutes, and none of it runs in
// this daemon: Merge starts a job (mergejob.go) in a service of its own and
// says so; how it went reaches the lead as a message once the job has
// ended, whatever happened to the daemon meanwhile.
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
	if s.Jobs == nil {
		return "", fmt.Errorf("thread #%d cannot be merged here: this daemon runs no jobs (%s)", threadID, s.JobsOff)
	}
	busy, err := s.merging(th)
	if err != nil {
		return "", fmt.Errorf("thread #%d: whether a merge of it is running could not be read, so none is started: %w", threadID, err)
	}
	if busy {
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
	if err := s.startMerge(th, lead, members, work, by); err != nil {
		return "", err
	}
	return fmt.Sprintf("Merging thread #%d: %s. Each repo's checks run now, which takes minutes. "+
		"The result comes to the lead as a message; change nothing in a worktree meanwhile.", threadID, repoList(work)), nil
}

// merging reports whether a thread's merge is under way: its row says so,
// or a merge job of it has not ended. Either alone is enough to refuse a
// second one. When the jobs cannot be read that is an error, and never an
// answer: "it is being merged, wait for the message" would be a promise of
// a message nobody sends.
func (s *Service) merging(th store.Thread) (bool, error) {
	if th.State == store.ThreadMerging {
		return true, nil
	}
	job, err := s.Jobs.OpenJob(mergeSubject(th.ID))
	if err != nil {
		log.Printf("threads: #%d: its merge job was not read, so no merge is started: %v", th.ID, err)
		return false, err
	}
	return job != nil, nil
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

// asked opens the word to the lead on a merge it did not ask for itself.
func asked(by string) string {
	if by == ByPerson {
		return "[ccmux] A person asked for this thread's merge on its page.\n\n"
	}
	return ""
}

// settle records how a merge ended. A row that was not written is logged
// and returned: whoever can try again must, because a thread left saying
// "merging" with no merge running refuses every merge after it.
func (s *Service) settle(th store.Thread, state, note string) error {
	th.State, th.Note, th.UpdatedAt = state, note, s.millis()
	err := s.St.SaveThread(th)
	if err != nil {
		log.Printf("threads: #%d: its merge's end (%s) was not recorded: %v", th.ID, state, err)
	}
	return err
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

// failedText is what the lead reads of a merge that failed. landed is the
// trunks the merge had moved by then, which the failure's own words may
// not name when the merge was cut off and run again in between.
func failedText(th store.Thread, work []store.RunnerSession, err error, landed []string) string {
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
	return fmt.Sprintf("[ccmux] Thread #%d: the merge failed, and the thread stays open with its team as it was.\n\n%s%s\n\n"+
		"A failed check was run twice before it came to you; where a file is named above it holds that check's whole output. %s",
		th.ID, err.Error(), halfIn(err.Error(), landed), whose)
}

// halfIn names the trunks a failed merge had already moved, unless the
// failure's own words do.
func halfIn(said string, landed []string) string {
	if len(landed) == 0 || strings.Contains(said, "already merged: ") {
		return ""
	}
	return "\n\nAlready merged, and staying so: " + strings.Join(landed, ", ") + "."
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
