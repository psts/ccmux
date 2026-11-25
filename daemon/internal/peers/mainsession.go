// A repo's plain name and its main session. A repo's main session (its
// session on the thread page: a runner session of kind lead, in no thread)
// is on the bus as "<repo>-s-<id>", never under the repo folder's plain
// name: a peer registered under exactly that name (a terminal Claude a
// person started in the repo) keeps answering to it. But where every Claude
// is a session nobody is registered under the plain name, and whoever names
// a repo and is not routed to a helper of its own (helperroute.go: a main
// session, a thread member) means the repo's session: a role agent, a task
// builder, a helper naming its own repo.
//
// So a by-name message that finds no present peer called exactly that goes
// to the main session of the repo whose folder is called that, in the
// addressed group. It is present for as long as it has not ended, and the
// message wakes it through its inbox (runnerpeer.go). With no main session,
// spawn_if_missing starts one (StartMain) and the message is its first
// prompt: never a terminal Claude where sessions run. A host with no
// session runners has no main sessions and no StartMain, and keeps the tmux
// spawn (spawn.go) as it was.
package peers

import (
	"fmt"
	"path/filepath"
	"time"
)

// mainRepoLocked is the plain name a peer answers to as its repo's main
// session: the repo folder's name, the same match the spawn plan makes
// (manager.repoNamed). "" for every other peer.
func (s *Service) mainRepoLocked(p *Peer) string {
	info, ok := s.runnerLocked(p.PaneID)
	if !ok || !info.Main || info.Repo == "" {
		return ""
	}
	return filepath.Base(info.Repo)
}

// answersToLocked reports whether name reaches p: its own name on the bus,
// or its repo's plain name when it is that repo's main session.
func (s *Service) answersToLocked(p *Peer, name string) bool {
	return p.Name == name || s.mainRepoLocked(p) == name
}

// mainSessionsLocked is the main sessions of the repos called name in
// group: one, unless two repos of the group share a folder name.
func (s *Service) mainSessionsLocked(name, group string) []*Peer {
	var out []*Peer
	for _, p := range s.peers {
		if name != "" && s.mainRepoLocked(p) == name && sameGroup(s.groupOfLocked(p), group) {
			out = append(out, p)
		}
	}
	return out
}

// answeringLocked is who name reaches in group: the present peers called
// exactly that, the sender left out, and with none the main session of the
// repo called that. self says the only one there is the sender: a main
// session naming its own repo.
func (s *Service) answeringLocked(name, group, senderID string) (matches []*Peer, self bool) {
	if matches = s.namedLocked(name, group, senderID); len(matches) > 0 {
		return matches, false
	}
	for _, p := range s.mainSessionsLocked(name, group) {
		if p.ID == senderID {
			self = true
			continue
		}
		matches = append(matches, p)
	}
	return matches, self && len(matches) == 0
}

// selfNameError is what a main session is told when it names its own repo:
// nothing is started (the session that would answer is the one asking).
func selfNameError(name string) string {
	return fmt.Sprintf("%q is you: this session is that repo's main session, and a message to the repo's name would come back to it. "+
		"Name another repo, or a peer by its id (see list_peers)", name)
}

// plainNameLocked is the plain name a listing shows p answering to: its
// repo's, when it is the main session and no present peer in its group is
// called exactly that (that peer would get the message instead).
func (s *Service) plainNameLocked(p *Peer, group string) string {
	plain := s.mainRepoLocked(p)
	if plain == "" || plain == p.Name || len(s.namedLocked(plain, group, p.ID)) > 0 {
		return ""
	}
	return plain
}

// mainStartLocked is the repo session in group whose folder is called
// name, when spawn_if_missing would start its main session: this host runs
// sessions, and name is a repo of the group (open or closed, the lookups
// the tmux spawn makes). ok is false anywhere else.
func (s *Service) mainStartLocked(group, name string) (wsID, repo string, ok bool) {
	if s.StartMain == nil || s.RunnerInbox == nil {
		return "", "", false
	}
	if wsID, repo, ok = s.mgr.LiveWorkspaceForRepo(group, name); ok {
		return wsID, repo, true
	}
	return s.mgr.ClosedWorkspaceForRepo(group, name)
}

// tryStartMainLocked is the repo branch of spawn_if_missing where sessions
// run: the repo's main session is made, off the lock, and the message
// waits for its peer like a spawn's (two messages wait for the one start).
// ok is false when this is not that case, and the tmux spawn goes on.
func (s *Service) tryStartMainLocked(sender *Peer, key, group, name, text string) (SendResp, bool) {
	wsID, repo, ok := s.mainStartLocked(group, name)
	if !ok {
		return SendResp{}, false
	}
	pending := &pendingSpawn{
		name: name, group: group, repo: repo, main: true,
		requests: []queuedRequest{{fromID: sender.ID, text: text}},
	}
	pending.timer = time.AfterFunc(s.SpawnTimeout, func() { s.spawnTimedOut(key) })
	s.spawns[key] = pending
	go s.startMain(key, pending, wsID)
	return SendResp{OK: true, Spawning: true}, true
}

// startMain makes the main session a message waits for. Its peer coming
// onto the bus (EnsureRunnerPeer, which the daemon calls when the session
// is made) hands the message over. It is called here once more, for a main
// session that was there already with no peer on the bus; a start that
// still left the message waiting is said to whoever sent it, at once.
func (s *Service) startMain(key string, pending *pendingSpawn, wsID string) {
	paneID, cwd, err := s.StartMain(wsID)
	if err != nil {
		s.abortSpawn(key, fmt.Sprintf("%q's session could not be started: %v", pending.name, err))
		return
	}
	s.EnsureRunnerPeer(paneID, cwd)
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.spawns[key] == pending {
		s.abortSpawnLocked(key, fmt.Sprintf("%q's session was made but is not on the bus, so your message did not reach it. Open it on the thread page", pending.name))
	}
}

// pendingMainLocked is the start a main session's peer was awaited by: the
// one queued under its repo's plain name in its group.
func (s *Service) pendingMainLocked(p *Peer, group string) (string, *pendingSpawn) {
	plain := s.mainRepoLocked(p)
	if plain == "" {
		return "", nil
	}
	key := spawnKey(group, plain)
	if pending := s.spawns[key]; pending != nil && pending.main {
		return key, pending
	}
	return "", nil
}
