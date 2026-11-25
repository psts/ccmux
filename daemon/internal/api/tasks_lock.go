package api

import (
	"errors"
	"fmt"
	"log"
	"net/http"
	"strings"
	"time"

	"ccmux.dev/ccmuxd/internal/store"
	"ccmux.dev/ccmuxd/internal/taskq"
)

// clearTaskLock: POST /v1/tasks/{id}/clear-lock, a person's "Remove the
// stale lock" on a built feature whose merge waits on a git lock file a
// crashed git left. For each of the task's repos the daemon looks only at
// the lock git itself names there (nothing in the request names a path)
// and removes it only when taskq.ClearStaleLock's checks all hold. It
// starts no merge: the runner's next pass merges as usual. Gated by the
// API token like every /v1 route.
func (s *Server) clearTaskLock(w http.ResponseWriter, r *http.Request) {
	if !s.tasksReady(w) {
		return
	}
	s.tasksMu.Lock()
	defer s.tasksMu.Unlock()
	t, _, ok := s.taskOf(w, r)
	if !ok {
		return
	}
	if t.Status != taskq.ReadyToMerge {
		writeError(w, http.StatusConflict, fmt.Sprintf("task #%d is %s; a lock is removed only for a built task waiting to merge", t.ID, t.Status))
		return
	}
	parts, err := s.tasks.Parts()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	removed, err := clearLocks(taskq.PartsOf(parts, t.ID), time.Now())
	if err != nil {
		log.Printf("tasks: #%d: stale lock not removed: %v", t.ID, err)
		writeError(w, http.StatusConflict, err.Error())
		return
	}
	log.Printf("tasks: #%d: a person removed %s; the next pass merges it", t.ID, strings.Join(removed, ", "))
	if s.taskSettler != nil {
		s.taskSettler.ForgetBlock(t.ID)
	}
	s.mgr.TasksChanged()
	writeJSON(w, http.StatusOK, map[string]any{"id": t.ID, "removed": removed})
}

// clearLocks removes every stale lock in the parts' repos, each repo once.
// A repo with no lock is passed over; a lock that may not go is the
// refusal, with nothing after it tried. None at all is a refusal too.
func clearLocks(parts []store.Part, now time.Time) ([]string, error) {
	var removed []string
	seen := map[string]bool{}
	for _, p := range parts {
		if seen[p.RepoPath] {
			continue
		}
		seen[p.RepoPath] = true
		lock, err := taskq.ClearStaleLock(p.RepoPath, now)
		if errors.Is(err, taskq.ErrNoLock) {
			continue
		}
		if err != nil {
			return removed, err
		}
		removed = append(removed, lock)
	}
	if len(removed) == 0 {
		return nil, errors.New("there is no git lock file in this task's repos (any more): nothing to remove")
	}
	return removed, nil
}
