// Thread routing. A thread with helpers (internal/threads) is one piece of
// work with its own team: the session that leads it, and a helper session
// of its own for every other repo it needs, even when that repo has a live
// session. So when a thread member names a repo of its window, the message
// goes to the thread's helper for that repo, stood up on the spot when it is
// not there, and never to the repo's own session.
//
// Helpers carry unique bus names (<repo>-thread-<id>), so the repo's own
// session never shares a name with one and never gets the thread's mail.
// Senders outside a thread, names that are no repo of the window, and a
// message into another group all keep the routing they always had. A member
// still reaches the repo's own session, or anyone else, by id.
package peers

import (
	"fmt"
	"time"
)

// HelperRoute is where a thread member's message to a repo goes: Name is
// the bus name of the thread's member for that repo, Repo its folder, and
// ThreadID the thread, for standing the helper up.
type HelperRoute struct {
	ThreadID int64
	Repo     string
	Name     string
}

// threadTargetLocked routes a by-name message from a thread member to its
// thread's member for the named repo. routed is false when thread routing
// has no say; then the usual by-name lookup goes on. A helper that is not
// on the bus is started, and the message waits for it like a spawn's.
func (s *Service) threadTargetLocked(req SendReq, sender *Peer, group string) (target *Peer, resp SendResp, routed bool) {
	if s.ThreadRoute == nil || sender.PaneID == "" {
		return nil, SendResp{}, false
	}
	if req.ToGroup != "" && !sameGroup(req.ToGroup, group) {
		return nil, SendResp{}, false // another project was named: the thread has no helpers there
	}
	route, ok := s.ThreadRoute(sender.PaneID, req.ToName)
	if !ok {
		return nil, SendResp{}, false
	}
	if there := s.namedLocked(route.Name, group, sender.ID); len(there) == 1 {
		return there[0], SendResp{}, true
	}
	return nil, s.startHelperLocked(sender, group, route, req.Text), true
}

// startHelperLocked stands a thread's helper up, or joins a start already
// under way, and queues the message for it. The helper's session is made
// off the lock; its peer registering (EnsureRunnerPeer) delivers what
// waited, and a start that fails or never registers tells the sender, as a
// teammate spawn does.
func (s *Service) startHelperLocked(sender *Peer, group string, route HelperRoute, text string) SendResp {
	if s.StartHelper == nil {
		return SendResp{Error: fmt.Sprintf("this thread's helper %q is not running, and this daemon cannot start one", route.Name)}
	}
	key := spawnKey(group, route.Name)
	if pending := s.spawns[key]; pending != nil {
		pending.requests = append(pending.requests, queuedRequest{fromID: sender.ID, text: text})
		return SendResp{OK: true, Spawning: true}
	}
	pending := &pendingSpawn{name: route.Name, group: group, repo: route.Repo, requests: []queuedRequest{{fromID: sender.ID, text: text}}}
	pending.timer = time.AfterFunc(s.SpawnTimeout, func() { s.spawnTimedOut(key) })
	s.spawns[key] = pending
	go func() {
		if err := s.StartHelper(route.ThreadID, route.Repo); err != nil {
			s.abortSpawn(key, fmt.Sprintf("Helper %q could not be started: %v", route.Name, err))
		}
	}()
	return SendResp{OK: true, Spawning: true}
}
