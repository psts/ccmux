package api

import (
	"log"
	"path/filepath"

	"ccmux.dev/ccmuxd/internal/agent"
	"ccmux.dev/ccmuxd/internal/buscontext"
	"ccmux.dev/ccmuxd/internal/manager"
	"ccmux.dev/ccmuxd/internal/store"
)

// A person moved a repo to another window (PUT /v1/workspaces/{id}/group).
// The bus regroups its sessions at once (GroupForPane is read live), but
// each Claude was told its window's sessions, agents and shared folder once,
// at start. So the move settles here, in this order: the old window's
// shared folder is taken back, the new one's is granted, and the repo's
// running sessions start again to read the new window. A runner session
// restarts and resumes once its turn ends (runner.RestartBetweenTurns); a
// tmux Claude restarts only when it sits idle at an empty prompt
// (manager.RestartMoved), and is told otherwise.
//
// Only this route settles a move. The membership hook (windownotices.go)
// also fires for a create, a delete, an agent's start and the import of a
// legacy group on a plain list read; none of those is a move of a running
// repo. An agent is never settled here: its home, schedules and memory
// belong to the window it was added to.

// movedRepo is what a settle needs of the workspace, copied at the move.
type movedRepo struct {
	id, name, repoPath string
	from               manager.WindowInfo // the window it left; ID "" for none
	to                 string             // the window it is in now, "" for none
}

// windowOfWorkspace is the window wsID is a member of, and false for none.
func (s *Server) windowOfWorkspace(wsID string) (manager.WindowInfo, bool, error) {
	windows, err := s.mgr.WindowsListStrict()
	if err != nil {
		return manager.WindowInfo{}, false, err
	}
	for _, w := range windows {
		for _, id := range w.WorkspaceIDs {
			if id == wsID {
				return w, true, nil
			}
		}
	}
	return manager.WindowInfo{}, false, nil
}

// repoMove reads what a settle needs before the move is made: nil for an
// agent's home, a workspace on another host, or a window state that cannot
// be read (logged; the move itself goes on).
func (s *Server) repoMove(wsID string) *movedRepo {
	ws := s.mgr.Workspace(wsID)
	if ws == nil || ws.Agent != "" {
		return nil
	}
	from, _, err := s.windowOfWorkspace(wsID)
	if err != nil {
		log.Printf("repo move %s: windows unreadable, its sessions are not restarted: %v", ws.Name, err)
		return nil
	}
	return &movedRepo{id: wsID, name: ws.Name, repoPath: ws.RepoPath, from: from}
}

// settleMove runs after the move was saved. A move to the same window is
// none. The shared folders are settled before the route answers (two small
// file writes), so no restart can come up before its new grant; the
// restarts run after it, since a tmux one waits for Claude to exit.
func (s *Server) settleMove(mv *movedRepo) {
	if mv == nil {
		return
	}
	// The strict read: the cached snapshot goes empty on a failed store
	// read, which would settle a move into a window as a move into none.
	to, found, err := s.windowOfWorkspace(mv.id)
	if err != nil {
		log.Printf("repo move %s: windows unreadable after the move, its sessions and shared folders are not settled: %v", mv.name, err)
		return
	}
	if found {
		mv.to = to.Name
	}
	if mv.to == mv.from.Name {
		return
	}
	s.moveAccess(mv)
	go s.restartMovedRepo(mv)
}

// moveAccess takes the old window's shared folder back from the repo and
// grants the new one's. The old grant stays while another session of the
// same repo folder is still in the old window: the grant is the folder's,
// in its .claude/settings.local.json, not the session's.
func (s *Server) moveAccess(mv *movedRepo) {
	if s.agents == nil || mv.repoPath == "" {
		return
	}
	bus := repoBusName(mv.name, mv.repoPath)
	if mv.from.ID != "" && !s.repoStillIn(mv.from.ID, mv.repoPath) {
		dir := filepath.Join(s.agents.SharedDir(mv.from.ID, mv.from.Name), bus)
		if wrote, err := agent.RevokeRepoDir(mv.repoPath, dir); err != nil {
			log.Printf("repo move %s: shared folder %s not taken back: %v", mv.name, dir, err)
		} else if wrote {
			log.Printf("repo move %s: shared folder %s taken back (its .claude/settings.local.json)", mv.name, dir)
		}
	}
	if mv.to == "" {
		return
	}
	if win, _, msg := s.windowByName(mv.to); msg == "" {
		s.grantRepoShared(win, buscontext.Session{Name: bus, RepoPath: mv.repoPath})
	}
}

// repoStillIn reports another workspace of the same repo folder in window
// windowID. Unreadable counts as yes: a grant left in place costs less than
// one taken from a session that still needs it.
func (s *Server) repoStillIn(windowID, repoPath string) bool {
	windows, err := s.mgr.WindowsListStrict()
	if err != nil {
		log.Printf("repo move %s: windows unreadable, the old shared folder's grant is kept: %v", repoPath, err)
		return true
	}
	for _, w := range windows {
		if w.ID != windowID {
			continue
		}
		for _, id := range w.WorkspaceIDs {
			if ws := s.mgr.Workspace(id); ws != nil && ws.Agent == "" && ws.RepoPath == repoPath {
				return true
			}
		}
	}
	return false
}

// restartMovedRepo starts the repo's running sessions again, each between
// two turns so no reply is cut. A task's builder is left alone: its task
// stays in the old window's queue, and a restart would cut its work for
// nothing. An asleep session reads the new window when it next wakes.
func (s *Server) restartMovedRepo(mv *movedRepo) {
	for _, id := range s.movedRunners(mv.id) {
		if err := s.runners.RestartBetweenTurns(id); err != nil {
			log.Printf("repo move %s: session %s not restarted: %v", mv.name, id, err)
		} else {
			log.Printf("repo move %s: session %s restarts once its turn ends, to read %s", mv.name, id, windowWord(mv.to))
		}
	}
	for _, pane := range s.mgr.ClaudePanes(mv.id) {
		go s.restartMovedPane(mv, pane)
	}
}

// movedRunners are the workspace's runner sessions a move restarts.
func (s *Server) movedRunners(wsID string) []string {
	if s.runners == nil || s.runnerRows == nil {
		return nil
	}
	rows, err := s.runnerRows.RunnerSessions()
	if err != nil {
		log.Printf("repo move: session rows unreadable, none restarted: %v", err)
		return nil
	}
	var out []string
	for _, r := range rows {
		if r.WorkspaceID == wsID && r.Wanted == store.SessionRunning && r.Role != store.RoleBuilder {
			out = append(out, r.ID)
		}
	}
	return out
}

// restartMovedPane restarts a tmux Claude, or tells it why it was not.
func (s *Server) restartMovedPane(mv *movedRepo, pane string) {
	err := s.mgr.RestartMoved(pane)
	if err == nil {
		log.Printf("repo move %s: pane %s restarted to read %s", mv.name, pane, windowWord(mv.to))
		return
	}
	log.Printf("repo move %s: pane %s not restarted: %v", mv.name, pane, err)
	if s.peersSvc == nil {
		return
	}
	if nerr := s.peersSvc.NotifyPane(pane, moveNote(mv)); nerr != nil {
		log.Printf("repo move %s: pane %s not told either: %v", mv.name, pane, nerr)
	}
}

// moveNote is what a session that could not be restarted is told.
func moveNote(mv *movedRepo) string {
	from := windowWord(mv.from.Name)
	return "Your repo was moved from " + from + " to " + windowWord(mv.to) + ". " +
		"The project sessions, agents and shared folder in your bus instructions are " + from + "'s and out of date: " +
		"list_peers shows who is in your window now. Restart this session when convenient to read them fresh."
}

// windowWord names a window in a sentence: "window X", or "no window".
func windowWord(name string) string {
	if name == "" {
		return "no window"
	}
	return "window " + name
}
