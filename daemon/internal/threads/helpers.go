package threads

import (
	"errors"
	"fmt"
	"log"
	"path/filepath"

	"ccmux.dev/ccmuxd/internal/model"
	"ccmux.dev/ccmuxd/internal/store"
	"ccmux.dev/ccmuxd/internal/taskq"
)

// Route is where a member's message to a repo goes: the bus name of its
// thread's member for that repo, the repo's folder, and the thread. Lead is
// the main session a side helper is for (side.go), whose side thread may
// have no row yet (ThreadID 0). Pane, when set, is the one session the
// message is for: a side helper's message to its main session.
type Route struct {
	ThreadID int64
	Repo     string
	Name     string
	Lead     string
	Pane     string
}

// Route answers the bus (peers/helperroute.go): the session in senderPane
// named toName. When the sender is a thread member and toName is another
// repo of its window (the repo's folder name, or its session's name), the
// message is for the thread's own member for that repo. Anything else, a
// sender in no thread, a name that is no repo here, the member's own repo
// (whose own session it may want), is not this thread's to route.
//
// The bus asks under its own lock, so this reads memory only.
func (s *Service) Route(senderPane, toName string) (Route, bool) {
	s.mu.RLock()
	me, ok := s.members[senderPane]
	s.mu.RUnlock()
	if !ok {
		return Route{}, false
	}
	ws := repoNamed(s.Host.WindowPeers(senderPane), toName)
	if ws == nil {
		return Route{}, false
	}
	if me.side != nil {
		return sideRoute(me, senderPane, ws)
	}
	if ws.RepoPath == me.repo {
		return Route{}, false
	}
	return Route{ThreadID: me.thread, Repo: ws.RepoPath, Name: taskq.ThreadBusName(ws.RepoPath, me.thread)}, true
}

// repoNamed picks the repo session called name, by its folder or by its
// session's own name; never a role agent's.
func repoNamed(list []*model.Workspace, name string) *model.Workspace {
	for _, ws := range list {
		if ws.Agent == "" && (filepath.Base(ws.RepoPath) == name || ws.Name == name) {
			return ws
		}
	}
	return nil
}

// reindex rebuilds who is in which thread from the rows. A registry that
// cannot be read keeps the last answer and says so: members that are there
// keep their routing.
func (s *Service) reindex() {
	threads, err := s.St.Threads()
	if err != nil {
		log.Printf("threads: the member index was not rebuilt, the last one stands: %v", err)
		return
	}
	next := map[string]memberRef{}
	sides := map[string]int64{} // a main session's id → its side thread
	for _, th := range threads {
		if th.Kind == store.ThreadSide {
			sides[th.LeadSession] = th.ID
		}
		members, err := s.St.ThreadMembers(th.ID)
		if err != nil {
			log.Printf("threads: the member index was not rebuilt, the last one stands: #%d: %v", th.ID, err)
			return
		}
		for _, m := range members {
			if m.PaneID != "" {
				next[m.PaneID] = memberRef{thread: th.ID, repo: m.Repo}
			}
		}
	}
	if err := s.indexSides(next, sides); err != nil {
		log.Printf("threads: the member index was not rebuilt, the last one stands: main sessions: %v", err)
		return
	}
	s.mu.Lock()
	s.members = next
	s.mu.Unlock()
}

// StartHelper stands up a thread's helper for repo: a fresh session in that
// repo's own folder, reading only, on the thread's model, under the name its
// teammates work out (<repo>-thread-<id>). The bus calls it for a message to
// a repo whose helper is not there, and delivers the message once the
// helper's peer is on the bus. Asked for a helper another message already
// started, it does nothing: the index in the registry keeps it to one.
func (s *Service) StartHelper(threadID int64, repo string) error {
	defer s.lock(threadKey(threadID))()
	th, lead, err := s.led(threadID)
	if err != nil {
		return err
	}
	ws := repoAt(s.Host.WindowPeers(lead.PaneID), repo)
	if ws == nil {
		return fmt.Errorf("%s is not a repo session in thread #%d's window", repo, threadID)
	}
	if _, err := s.Sessions.Create(memberSession(th, ws, store.RoleHelper)); err != nil && !errors.Is(err, store.ErrMemberExists) {
		return err
	}
	s.reopen(th)
	s.reindex()
	return nil
}

// repoAt picks the repo session whose folder is repo.
func repoAt(list []*model.Workspace, repo string) *model.Workspace {
	for _, ws := range list {
		if ws.Agent == "" && ws.RepoPath == repo {
			return ws
		}
	}
	return nil
}

// led reads a thread with its lead session. A thread whose lead has ended
// is over: nothing joins it and nothing of it is merged. A queued task's
// thread has no lead at all, and everything a lead may do to its thread is
// refused for it with that reason.
func (s *Service) led(threadID int64) (store.Thread, store.RunnerSession, error) {
	th, err := s.St.Thread(threadID)
	if err != nil {
		return store.Thread{}, store.RunnerSession{}, err
	}
	if th == nil {
		return store.Thread{}, store.RunnerSession{}, store.ErrNoThread
	}
	if th.TaskID != 0 {
		return *th, store.RunnerSession{}, taskOnly(*th) // the queue leads it (builders.go)
	}
	lead, err := s.St.RunnerSession(th.LeadSession)
	if err != nil {
		return *th, store.RunnerSession{}, err
	}
	if lead == nil || lead.Wanted == store.SessionDone {
		return *th, store.RunnerSession{}, fmt.Errorf("thread #%d is over: its lead session has ended", threadID)
	}
	return *th, *lead, nil
}

// threadKey names a thread's lock: one change to a thread's team at a time.
func threadKey(id int64) string { return fmt.Sprintf("thread:%d", id) }
