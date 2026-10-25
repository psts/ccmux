package api

import (
	"log"
	"os"
	"path/filepath"

	"ccmux.dev/ccmuxd/internal/agent"
	"ccmux.dev/ccmuxd/internal/buscontext"
	"ccmux.dev/ccmuxd/internal/manager"
)

// grantRepoShared gives a repo session of win its own folder in the
// window's shared folder, shared/<its bus name>/, writable from its next
// start: the folder is made, and the repo's local Claude settings list it
// (agent.GrantRepoDir). Failures are logged, never fatal: without the grant
// the session still writes there, after asking its human.
func (s *Server) grantRepoShared(win manager.WindowInfo, e buscontext.Session) {
	if s.agents == nil || e.RepoPath == "" {
		return
	}
	shared, err := s.agents.EnsureShared(win.ID, win.Name)
	if err != nil {
		log.Printf("shared folder for %s in %q: %v", e.Name, win.Name, err)
		return
	}
	dir := filepath.Join(shared, e.Name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		log.Printf("shared folder for %s in %q: %v", e.Name, win.Name, err)
		return
	}
	if wrote, err := agent.GrantRepoDir(e.RepoPath, dir); err != nil {
		log.Printf("shared folder for %s not granted: %v", e.Name, err)
	} else if wrote {
		log.Printf("shared folder %s granted to %s (its .claude/settings.local.json)", dir, e.RepoPath)
	}
}

// GrantSharedFolders grants every repo session of every window its shared
// folder: run once at startup, so sessions that joined before this existed
// get theirs too. Joins after that are granted as they happen.
func (s *Server) GrantSharedFolders() {
	windows, err := s.mgr.WindowsListStrict()
	if err != nil {
		log.Printf("shared folders: windows unreadable, none granted: %v", err)
		return
	}
	for _, win := range windows {
		for _, e := range s.windowSessions(win) {
			s.grantRepoShared(win, e)
		}
	}
}

// grantOnJoin is the join side: a repo session that just joined a window.
func (s *Server) grantOnJoin(c manager.MembershipChange) {
	if c.To == "" || c.Agent != "" {
		return
	}
	win, _, msg := s.windowByName(c.To)
	if msg != "" {
		return
	}
	s.grantRepoShared(win, buscontext.Session{Name: repoBusName(c.Name, c.RepoPath), RepoPath: c.RepoPath})
}
