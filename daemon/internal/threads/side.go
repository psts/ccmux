package threads

import (
	"errors"
	"fmt"
	"log"
	"path/filepath"

	"ccmux.dev/ccmuxd/internal/model"
	"ccmux.dev/ccmuxd/internal/runner"
	"ccmux.dev/ccmuxd/internal/store"
	"ccmux.dev/ccmuxd/internal/taskq"
)

// Side threads. A repo's main session (its session on the thread page, of
// kind lead and in no thread) is where a person plans and checks, and from
// where work is filed as a task or done at once. When it names another repo
// of its window ("ask backend"), the message goes to a helper of its own for
// that repo: a fresh session that reads, runs checks in a scratch copy when
// it has to, and hands real changes back. Never to the repo's old terminal
// session, and never to that repo's own main session, whose conversation is
// its person's.
//
// The helpers are members of the main session's side thread (kind
// store.ThreadSide), whose row is made with the first of them. It is never
// merged. A helper ends once its prompt cache has gone cold (ExpireSide):
// the next question starts a fresh one, which costs no more than a cold
// cache would have.

// SideHelperName is the bus name of the helper for repo that the main
// session of leadRepo asks: "backend-for-hq".
func SideHelperName(repo, leadRepo string) string {
	return filepath.Base(repo) + "-for-" + filepath.Base(leadRepo)
}

// sideRef is what the member index knows of a side thread's team: its main
// session, that session's pane, and its repo.
type sideRef struct {
	lead, pane, repo string
}

// sideRoute is where a message from a side thread's team to repo ws goes.
// The main session's own repo is the main session itself (for a helper) or
// not the thread's to route (for the main session). A helper's own repo is
// not either: it may want that repo's own session.
func sideRoute(me memberRef, senderPane string, ws *model.Workspace) (Route, bool) {
	if ws.RepoPath == me.side.repo {
		if senderPane == me.side.pane {
			return Route{}, false
		}
		return Route{ThreadID: me.thread, Repo: ws.RepoPath, Pane: me.side.pane}, true
	}
	if ws.RepoPath == me.repo {
		return Route{}, false
	}
	return Route{ThreadID: me.thread, Repo: ws.RepoPath, Lead: me.side.lead, Name: SideHelperName(ws.RepoPath, me.side.repo)}, true
}

// indexSides adds every main session's pane to the member index, with its
// side thread when it has one (0 when its first helper has not been asked
// for yet). Side helpers are in the index already, as their thread's
// members; they get the same side reference.
func (s *Service) indexSides(next map[string]memberRef, sides map[string]int64) error {
	mains, err := s.St.MainSessions()
	if err != nil {
		return err
	}
	refs := map[int64]*sideRef{}
	for _, m := range mains {
		ws := s.Host.Workspace(m.WorkspaceID)
		if ws == nil || m.PaneID == "" {
			continue
		}
		side := &sideRef{lead: m.ID, pane: m.PaneID, repo: ws.RepoPath}
		next[m.PaneID] = memberRef{thread: sides[m.ID], repo: ws.RepoPath, side: side}
		if id := sides[m.ID]; id != 0 {
			refs[id] = side
		}
	}
	for pane, ref := range next {
		if side := refs[ref.thread]; side != nil && ref.side == nil {
			ref.side = side
			next[pane] = ref
		}
	}
	return nil
}

// NoteMain hears of a main session's row (made, woken, renamed): one the
// member index does not have yet is added, so its first message to another
// repo reaches a side helper.
func (s *Service) NoteMain(sess store.RunnerSession) {
	s.mu.RLock()
	_, known := s.members[sess.PaneID]
	s.mu.RUnlock()
	if !known && sess.PaneID != "" {
		s.reindex()
	}
}

// StartSideHelper stands up main session leadID's helper for repo: its side
// thread's row first when it has none, then a fresh session in repo's own
// folder, reading only, on the main session's model, under the name the bus
// worked out (SideHelperName). Asked for a helper another message already
// started, it does nothing: the index in the registry keeps it to one.
func (s *Service) StartSideHelper(leadID, repo string) error {
	defer s.lock("side:" + leadID)()
	lead, err := s.mainSession(leadID)
	if err != nil {
		return err
	}
	home := s.Host.Workspace(lead.WorkspaceID)
	ws := repoAt(s.Host.WindowPeers(lead.PaneID), repo)
	if home == nil || ws == nil {
		return fmt.Errorf("%s is not a repo session in the window of %s's main session", repo, leadID)
	}
	th, err := s.sideThread(lead)
	if err != nil {
		return err
	}
	if _, err := s.Sessions.Create(sideMember(th, ws, lead, home.RepoPath)); err != nil && !errors.Is(err, store.ErrMemberExists) {
		return err
	}
	s.reopen(th)
	s.reindex()
	return nil
}

// mainSession reads a repo's main session that has not ended.
func (s *Service) mainSession(id string) (store.RunnerSession, error) {
	sess, err := s.St.RunnerSession(id)
	if err != nil {
		return store.RunnerSession{}, err
	}
	if sess == nil || sess.Kind != store.SessionLead || sess.ThreadID != 0 || sess.Wanted == store.SessionDone {
		return store.RunnerSession{}, fmt.Errorf("session %s is not a repo's main session that runs", id)
	}
	return *sess, nil
}

// sideThread is the main session's side thread, made when it has none.
func (s *Service) sideThread(lead store.RunnerSession) (store.Thread, error) {
	th, err := s.St.SideThread(lead.ID)
	if err != nil || th != nil {
		if th == nil {
			return store.Thread{}, err
		}
		return *th, err
	}
	window, err := s.Host.WindowOfWorkspace(lead.WorkspaceID)
	if err != nil {
		return store.Thread{}, fmt.Errorf("reading the main session's window: %w", err)
	}
	now := s.millis()
	return s.St.AddThread(store.Thread{Group: window, LeadSession: lead.ID, Kind: store.ThreadSide, State: store.ThreadOpen, CreatedAt: now, UpdatedAt: now})
}

// sideMember is the session a side helper starts as.
func sideMember(th store.Thread, ws *model.Workspace, lead store.RunnerSession, leadRepo string) runner.NewSession {
	return runner.NewSession{
		Cwd: ws.RepoPath, Name: SideHelperName(ws.RepoPath, leadRepo), Model: lead.Model, WorkspaceID: ws.ID,
		Title: "Helping " + filepath.Base(leadRepo), ThreadID: th.ID, Role: store.RoleHelper, Repo: ws.RepoPath, ReadOnly: true,
	}
}

// isSide reports whether thread id is a side thread. A row that cannot be
// read is not one: the caller then does what it does for any thread.
func (s *Service) isSide(id int64) bool {
	th, err := s.St.Thread(id)
	return err == nil && th != nil && th.Kind == store.ThreadSide
}

// dropScratch removes a side helper's scratch copy and its branch, whatever
// is in them: nothing in a scratch copy is ever merged, and the helper was
// told so. It returns what stayed.
func (s *Service) dropScratch(m store.RunnerSession) []string {
	drop := s.DropScratch
	if drop == nil {
		drop = taskq.DropScratch
	}
	if err := drop(m.Repo, m.Worktree, m.Branch); err != nil {
		log.Printf("threads: #%d helper %s: its scratch copy stayed: %v", m.ThreadID, m.Name, err)
		return []string{fmt.Sprintf("%s (%v)", m.Worktree, err)}
	}
	return nil
}

// mainEnded stands down the helpers of a main session that has ended, and
// closes its side thread: nothing would ever ask them anything again. What
// fails here is done again by the expiry pass (ExpireSide), which finds a
// side thread whose main session ended and is not closed.
func (s *Service) mainEnded(sess store.RunnerSession) {
	if sess.Kind != store.SessionLead {
		return // a session in no thread that is no main session has no helpers
	}
	th, err := s.St.SideThread(sess.ID)
	if err != nil || th == nil {
		if err != nil {
			log.Printf("threads: main session %s ended and its side thread was not read: %v", sess.ID, err)
		}
		return
	}
	s.closeSide(*th, "ended", true)
}

// MainMovedOn stands down main session leadID's helpers because it moved on
// to other work: it started a new conversation (a clean start or Start
// over, heard from the runner) or filed a task. What its helpers know is about the work before, and the
// next question to a repo starts a fresh helper. why says what it did, for
// the log. A session with no side thread, or one already closed, has
// nothing to stand down.
func (s *Service) MainMovedOn(leadID, why string) {
	th, err := s.St.SideThread(leadID)
	if err != nil {
		log.Printf("threads: main session %s %s; its side thread was not read, its helpers end when their cache is cold: %v", leadID, why, err)
		return
	}
	if th == nil || th.State == store.ThreadEnded {
		return
	}
	log.Printf("threads: #%d: main session %s %s; its helpers are stood down", th.ID, leadID, why)
	s.closeSide(*th, why, false)
}

// closeSide stands every helper of a side thread down and marks it closed,
// because its main session did what why says. retried says whether a
// stand-down that fails is done again: the expiry pass walks a side thread
// whose main session ended, never one whose main session runs on, whose
// helpers then end when their cache is cold.
func (s *Service) closeSide(th store.Thread, why string, retried bool) {
	defer s.reindex()
	defer s.lock(threadKey(th.ID))()
	left, err := s.standAllDown(th.ID)
	if err != nil {
		next := "they end when their cache is cold"
		if retried {
			next = "tried again on the next pass"
		}
		log.Printf("threads: #%d: its main session %s, and its helpers were not stood down (%s): %v", th.ID, why, next, err)
		return
	}
	if retried && s.helperStillRuns(th.ID) {
		log.Printf("threads: #%d: its main session %s, and a helper did not stop (tried again on the next pass)%s", th.ID, why, stayed(left))
		return // left open, so the expiry pass walks it again
	}
	th.State, th.Note, th.UpdatedAt = store.ThreadEnded, stayedNote(left), s.millis()
	if err := s.St.SaveThread(th); err != nil {
		log.Printf("threads: #%d: its main session %s; its helpers are stood down but the thread is not marked closed: %v", th.ID, why, err)
	}
}

// helperStillRuns says a side thread still has a helper that has not ended,
// after a stand-down: one whose stop failed. A list that cannot be read
// counts as yes, so the thread stays open for the next pass.
func (s *Service) helperStillRuns(threadID int64) bool {
	helpers, err := s.helpers(threadID)
	if err != nil {
		log.Printf("threads: #%d: its helpers were not read after their stand-down: %v", threadID, err)
		return true
	}
	return len(helpers) > 0
}

// sideMoved is what a side helper reads about its scratch copy.
func sideMoved(sess store.RunnerSession) Moved {
	return Moved{
		Message: fmt.Sprintf("ccmux: this call was not run. You were reading in %s, the repo's own folder, which a helper never changes. "+
			"Your scratch copy is ready at %s. This session restarts there in a moment and tells you so: end your turn now, and run this there.",
			sess.Repo, sess.Worktree),
		Notice: fmt.Sprintf("[ccmux] You now work in a scratch copy of the repo: %s. The call that brought you here was not run. "+
			"Run your checks here (tests, builds, reads of live systems). Nothing here is ever merged, and the copy is deleted when you end: "+
			"if a real change is needed, do not make it, say exactly what it is in your answer, and the main session files it as a task.",
			sess.Worktree),
	}
}
