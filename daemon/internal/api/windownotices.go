package api

import (
	"log"
	"path/filepath"
	"strings"
	"time"

	"ccmux.dev/ccmuxd/internal/manager"
	"ccmux.dev/ccmuxd/internal/repoowns"
)

// Window notices: when a repo session or an agent joins or leaves a shared
// window, every other session there hears it once from the bus, so a team
// that started before admin existed learns admin is there to hand work to.
// A restart or a close is not a membership change and says nothing:
// delegate still reaches a closed session (spawn_if_missing reopens it).

// noticeOwnsWait bounds how long a join notice waits for the new repo's
// owns line (its first summary runs on the first read), and noticeOwnsPoll
// how often it looks.
var (
	noticeOwnsWait = 60 * time.Second
	noticeOwnsPoll = 3 * time.Second
)

// windowNoticeQueue bounds the changes waiting for the worker; past it a change
// is dropped with a log line rather than blocking the manager.
const windowNoticeQueue = 256

// WindowNotice is the manager's OnMembershipChange. It only queues: the
// manager calls it inline, and a join may wait up to noticeOwnsWait for an
// owns line. One worker sends them in the order they happened, so a leave
// is never overtaken by the join before it.
func (s *Server) WindowNotice(c manager.MembershipChange) {
	s.noticesOnce.Do(func() {
		s.notices = make(chan manager.MembershipChange, windowNoticeQueue)
		go func() {
			for c := range s.notices {
				s.sendNotice(c)
			}
		}()
	})
	select {
	case s.notices <- c:
	default:
		log.Printf("window notice: queue full; %s %q -> %q not announced", c.Name, c.From, c.To)
	}
}

// sendNotice is one change: a leave notice in the window left, a join
// notice in the window joined (a move is both). A join is dropped when the
// session is no longer in that window by the time its notice is ready.
func (s *Server) sendNotice(c manager.MembershipChange) {
	s.grantOnJoin(c)
	if s.peersSvc == nil {
		return
	}
	name := noticeName(c)
	if c.From != "" {
		s.peersSvc.NotifyGroup(c.From, name, leaveNotice(c, name))
	}
	if c.To == "" {
		return
	}
	text := s.joinNotice(c, name)
	if now := s.mgr.SharedGroupResolver()(c.WorkspaceID, ""); now != c.To {
		log.Printf("window notice: %s left %q before its join was told; not announced", name, c.To)
		return
	}
	s.peersSvc.NotifyGroup(c.To, name, text)
}

// noticeName is the name the bus knows the session by: an agent's own
// name, a repo session's folder (what it registers as, and what
// spawn_if_missing finds it by).
func noticeName(c manager.MembershipChange) string {
	if c.Agent != "" {
		return c.Agent
	}
	return repoBusName(c.Name, c.RepoPath)
}

// repoBusName is a repo session's name on the bus: its folder's name, or
// the workspace's when it has no folder.
func repoBusName(wsName, repoPath string) string {
	if repoPath == "" {
		return wsName
	}
	return filepath.Base(repoPath)
}

func (s *Server) joinNotice(c manager.MembershipChange, name string) string {
	if c.Agent != "" {
		about := ""
		if s.agents != nil {
			if d, err := s.agents.Get(c.Agent); err == nil && d.Description != "" {
				about = ": " + strings.TrimSuffix(d.Description, ".")
			}
		}
		return name + " (an agent) joined this window" + about +
			". Message it or delegate to it by name. No reply needed."
	}
	owns := ""
	if line := s.waitForOwns(name, c.RepoPath); line != "" {
		owns = ": owns " + line
	}
	return name + " joined this window" + owns + ". Hand any work in its repo to it with delegate (to_name=" + name +
		", spawn_if_missing=true) instead of doing it yourself. No reply needed."
}

func leaveNotice(c manager.MembershipChange, name string) string {
	if c.Agent != "" {
		return name + " (an agent) left this window. No reply needed."
	}
	return name + " left this window; nobody here owns its repo now. No reply needed."
}

// waitForOwns is the repo's owns line, waiting a little for the first
// summary a new repo gets; "" when there is none in time.
func (s *Server) waitForOwns(name, repoPath string) string {
	if s.owns == nil || repoPath == "" {
		return ""
	}
	if line := s.owns.Line(name, repoPath); line != "" {
		return line
	}
	if src, _ := repoowns.Source(repoPath); src == "" {
		return "" // no CLAUDE.md or README: no summary is coming
	}
	deadline := time.Now().Add(noticeOwnsWait)
	for {
		if line := s.owns.Line(name, repoPath); line != "" {
			return line
		}
		if !s.owns.Pending(repoPath) {
			// None running: it just finished (read once more), or it failed
			// recently (the repoowns log says why) and nothing is coming.
			if line := s.owns.Line(name, repoPath); line != "" {
				return line
			}
			log.Printf("window notice: no owns summary running for %s; announcing without one", name)
			return ""
		}
		if time.Now().After(deadline) {
			log.Printf("window notice: no owns line for %s yet; announcing without one", name)
			return ""
		}
		time.Sleep(noticeOwnsPoll)
	}
}
