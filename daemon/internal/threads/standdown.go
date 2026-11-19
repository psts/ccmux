package threads

import (
	"fmt"
	"log"
	"path/filepath"
	"strings"

	"ccmux.dev/ccmuxd/internal/store"
	"ccmux.dev/ccmuxd/internal/taskq"
)

// workOf is a member's branch as the merge sees it.
func workOf(m store.RunnerSession) taskq.Work {
	return taskq.Work{RepoPath: m.Repo, Worktree: m.Worktree, Branch: m.Branch, Label: fmt.Sprintf("#%d", m.ThreadID)}
}

// StandDown ends one of a thread's helpers, the one for the repo called
// repoName, and removes its worktree and branch when nothing in them would
// be lost. It returns what to say of it. The lead is never stood down: it
// ends with its own session.
func (s *Service) StandDown(threadID int64, repoName, by string) (string, error) {
	defer s.lock(threadKey(threadID))()
	_, lead, err := s.led(threadID)
	if err != nil {
		return "", err
	}
	helpers, err := s.helpers(threadID)
	if err != nil {
		return "", err
	}
	for _, h := range helpers {
		if filepath.Base(h.Repo) == repoName || h.Name == repoName {
			left := s.standDown(h)
			s.reindex()
			said := "Stood down " + h.Name + stayed(left) + "."
			s.tellOfPerson(lead, by, said)
			return said, nil
		}
	}
	return "", fmt.Errorf("thread #%d has no helper for %q (its helpers: %s)", threadID, repoName, names(helpers))
}

// helpers is a thread's members but its lead.
func (s *Service) helpers(threadID int64) ([]store.RunnerSession, error) {
	members, err := s.St.ThreadMembers(threadID)
	if err != nil {
		return nil, err
	}
	var out []store.RunnerSession
	for _, m := range members {
		if m.Role != store.RoleLead {
			out = append(out, m)
		}
	}
	return out, nil
}

func names(members []store.RunnerSession) string {
	if len(members) == 0 {
		return "none"
	}
	list := make([]string, len(members))
	for i, m := range members {
		list[i] = m.Name
	}
	return strings.Join(list, ", ")
}

// stayed is the words for what a stand-down had to leave.
func stayed(left []string) string {
	if len(left) == 0 {
		return ""
	}
	return "; left for you: " + strings.Join(left, "; ")
}

// standDown ends a helper's session and removes its worktree and branch,
// forcing nothing: a worktree with uncommitted files stays, and so does a
// branch with a commit its trunk lacks. It returns what stayed. A session
// that would not stop keeps its worktree: its Claude may still be in it.
func (s *Service) standDown(m store.RunnerSession) []string {
	if err := s.Sessions.SetWanted(m.ID, store.SessionDone); err != nil && !gone(err) {
		log.Printf("threads: #%d helper %s did not stop: %v", m.ThreadID, m.Name, err)
		return []string{fmt.Sprintf("%s (it did not stop: %v)", m.Name, err)}
	}
	if m.Worktree == "" {
		return nil
	}
	if why := s.Lander.Remove(workOf(m), taskq.RebaseMark(m.Worktree)); why != "" {
		log.Printf("threads: #%d helper %s stood down; %s", m.ThreadID, m.Name, why)
		return []string{why}
	}
	return nil
}

// tellOfPerson tells a thread's lead what a person did to its team on the
// thread's page. What the lead did itself, it knows.
func (s *Service) tellOfPerson(lead store.RunnerSession, by, said string) {
	if by == ByPerson {
		s.tell(lead, fmt.Sprintf("[ccmux] Thread #%d: a person did this on the thread's page. %s", lead.ThreadID, said))
	}
}

// End closes a thread: every helper is stood down, and the thread reads as
// ended. The lead's own session and work stay as they are: the lead says
// its last words, and a person ends its session. Work the lead takes up
// after that opens the thread again.
func (s *Service) End(threadID int64, by string) (string, error) {
	defer s.lock(threadKey(threadID))()
	th, lead, err := s.led(threadID)
	if err != nil {
		return "", err
	}
	if th.State == store.ThreadMerging {
		return "", fmt.Errorf("thread #%d is being merged; end it once the merge has answered, or stop the merge first", threadID)
	}
	left, err := s.standAllDown(threadID)
	if err != nil {
		return "", err
	}
	th.State, th.UpdatedAt = store.ThreadEnded, s.millis()
	if err := s.St.SaveThread(th); err != nil {
		return "", err
	}
	s.reindex()
	said := fmt.Sprintf("Thread #%d is closed and its helpers are stood down%s.", threadID, stayed(left))
	s.tellOfPerson(lead, by, said)
	return said, nil
}

// standAllDown stands every helper of a thread down and returns what stayed.
func (s *Service) standAllDown(threadID int64) ([]string, error) {
	helpers, err := s.helpers(threadID)
	if err != nil {
		return nil, err
	}
	var left []string
	for _, h := range helpers {
		left = append(left, s.standDown(h)...)
	}
	return left, nil
}

// SessionEnded hears that a session has ended (the supervisor's Changed,
// off its lock). A lead that ended takes its thread with it: its helpers
// are stood down, since nothing would ever merge or end them, and its own
// worktree goes when nothing in it would be lost. A helper that ended only
// leaves the team. A task's builder that ended takes nobody with it
// (builders.go).
func (s *Service) SessionEnded(sess store.RunnerSession) {
	if sess.ThreadID == 0 {
		return
	}
	if sess.Role == store.RoleBuilder {
		s.builderEnded(sess) // a task's part: its teammates are the queue's to end
		return
	}
	defer s.reindex()
	if sess.Role != store.RoleLead {
		return
	}
	defer s.lock(threadKey(sess.ThreadID))()
	left, err := s.standAllDown(sess.ThreadID)
	if err != nil {
		log.Printf("threads: #%d lost its lead and its helpers were not stood down: %v", sess.ThreadID, err)
		return
	}
	if sess.Worktree != "" {
		if why := s.Lander.Remove(workOf(sess), taskq.RebaseMark(sess.Worktree)); why != "" {
			left = append(left, why)
		}
	}
	th, err := s.St.Thread(sess.ThreadID)
	if err != nil || th == nil {
		return
	}
	th.State, th.Note, th.UpdatedAt = store.ThreadEnded, strings.TrimPrefix(stayed(left), "; "), s.millis()
	if err := s.St.SaveThread(*th); err != nil {
		log.Printf("threads: #%d ended with its lead but is not marked so: %v", th.ID, err)
	}
}
