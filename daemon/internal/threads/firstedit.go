package threads

import (
	"fmt"
	"log"
	"path/filepath"
	"time"

	"ccmux.dev/ccmuxd/internal/store"
	"ccmux.dev/ccmuxd/internal/taskq"
)

// Moved is the daemon's answer to a member's first change: Message is what
// the refused tool call is answered with, Notice what the member is told
// when it starts again in its worktree (the runner keeps it for that start).
type Moved struct {
	Message string `json:"message"`
	Notice  string `json:"notice"`
}

// FirstEdit moves a member out of its repo's own folder into its own
// worktree, on its thread's branch, because it is about to change something
// (claudeserve/readonly.mjs asks, from the tool call it refused). The
// worktree is made and the member's row moved before this answers; the
// member's runner is restarted there a moment after, so the answer reaches
// it first.
//
// Asked twice (Claude sends several edits in one message) it answers the
// same and restarts once. A member whose row has moved and whose runner
// still asks is still fenced in the repo's folder: the restart is on its
// way, or it never came (it failed, or the daemon stopped first), and then
// this ask is what brings it.
func (s *Service) FirstEdit(sessionID string) (Moved, error) {
	defer s.lock(sessionID)()
	sess, th, err := s.member(sessionID)
	if err != nil {
		return Moved{}, err
	}
	if sess.Role == store.RoleBuilder {
		// A builder has been in its worktree since it started and never asks;
		// a stray ask must not restart it in the middle of its work.
		return movedText(sess, ""), nil
	}
	if !sess.ReadOnly {
		s.relocateSoon(sessionID)
		return movedText(sess, ""), nil
	}
	worktree, branch := taskq.ThreadPlace(s.Root, sess.Repo, th.ID, s.titleNow(th))
	trunk, err := taskq.MakeWorktree(s.Lander.Git, sess.Repo, worktree, branch)
	if err != nil {
		return Moved{}, fmt.Errorf("making the worktree %s: %w", worktree, err)
	}
	place := store.SessionPlace{Cwd: worktree, Worktree: worktree, Branch: branch}
	if err := s.Sessions.Place(sessionID, place); err != nil {
		return Moved{}, err
	}
	s.reopen(th)
	sess.Cwd, sess.Worktree, sess.Branch, sess.ReadOnly = worktree, worktree, branch, false
	log.Printf("threads: #%d %s moves into %s on %s", th.ID, filepath.Base(sess.Repo), worktree, branch)
	s.relocateSoon(sessionID)
	return movedText(sess, trunk), nil
}

// relocateSoon restarts a member where its row now says it works, a moment
// from now, and once however often it is asked for meanwhile.
func (s *Service) relocateSoon(sessionID string) {
	if _, onItsWay := s.moving.LoadOrStore(sessionID, true); onItsWay {
		return
	}
	time.AfterFunc(s.MoveAfter, func() { s.relocate(sessionID) })
}

// titleNow is what a thread is called now, for its branch's name: its own
// title, else its lead session's, which its first message named.
func (s *Service) titleNow(th store.Thread) string {
	if th.Title != "" {
		return th.Title
	}
	if lead, err := s.St.RunnerSession(th.LeadSession); err == nil && lead != nil {
		return lead.Title
	}
	return ""
}

// relocate restarts a member where its row now says it works. A restart
// that fails leaves the member running where it was, still fenced, after it
// was told to end its turn and wait: so it is told, and its next change
// asks again (FirstEdit), which is the retry.
func (s *Service) relocate(sessionID string) {
	err := s.Sessions.Relocate(sessionID)
	s.moving.Delete(sessionID)
	if err == nil {
		return
	}
	log.Printf("threads: session %s was not restarted in its new folder: %v", sessionID, err)
	text := fmt.Sprintf("[ccmux] The restart into your worktree failed (%v), so this session still runs in the repo's own folder, read only. "+
		"Make your change again to try the move once more. If it fails again, say so in this thread: a person has to look.", err)
	if _, err := s.Sessions.Send(sessionID, text, sender); err != nil {
		log.Printf("threads: session %s could not be told its restart failed: %v", sessionID, err)
	}
}

// reopen makes a thread that had merged, or that its lead had closed, open
// again: it has new work.
func (s *Service) reopen(th store.Thread) {
	if th.State != store.ThreadMerged && th.State != store.ThreadEnded {
		return
	}
	th.State, th.UpdatedAt = store.ThreadOpen, s.millis()
	if err := s.St.SaveThread(th); err != nil {
		log.Printf("threads: #%d has new work but is not marked open again: %v", th.ID, err)
	}
}

// movedText is what a member reads about its worktree: in the answer to the
// call that was refused, and again when it starts there. trunk is "" for a
// member that had moved already.
func movedText(sess store.RunnerSession, trunk string) Moved {
	from := ""
	if trunk != "" {
		from = ", made from " + trunk
	}
	return Moved{
		Message: fmt.Sprintf("ccmux: this call was not run. You were reading in %s, the repo's own folder, which a thread member never changes. "+
			"Your own worktree is ready at %s on branch %s%s. This session restarts there in a moment and tells you so: end your turn now, and make this change there.",
			sess.Repo, sess.Worktree, sess.Branch, from),
		Notice: fmt.Sprintf("[ccmux] You now work in your own git worktree: %s, on branch %s%s. The call that brought you here was not run. "+
			"Every file you read in %s has its copy here under the same relative path: change the copies here, never that folder. "+
			"Commit on this branch; do not push, merge or switch branches. Go on with what you were about to change.",
			sess.Worktree, sess.Branch, from, sess.Repo),
	}
}
