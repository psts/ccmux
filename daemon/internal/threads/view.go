package threads

import (
	"log"
	"path/filepath"
	"strconv"

	"ccmux.dev/ccmuxd/internal/store"
	"ccmux.dev/ccmuxd/internal/taskq"
)

// Detail is a thread as its page shows it: its row, and each member with
// what its branch holds.
type Detail struct {
	Thread  store.Thread `json:"thread"`
	Members []Member     `json:"members"`
}

// Member is one member on the thread's page: its session, the repo it
// works for by name, and its changes (nil while it only reads). Status is
// the one line under its repo: "Reading only", or what its branch holds.
type Member struct {
	Session store.RunnerSession  `json:"session"`
	Repo    string               `json:"repo"`
	Status  string               `json:"status"`
	Changes *taskq.BranchChanges `json:"changes,omitempty"`
}

// Detail reads one thread with its members, the lead first.
func (s *Service) Detail(threadID int64) (Detail, error) {
	th, err := s.St.Thread(threadID)
	if err != nil {
		return Detail{}, err
	}
	if th == nil {
		return Detail{}, store.ErrNoThread
	}
	members, err := s.St.ThreadMembers(threadID)
	if err != nil {
		return Detail{}, err
	}
	out := Detail{Thread: *th, Members: make([]Member, len(members))}
	for i, m := range members {
		out.Members[i] = s.memberView(m)
	}
	return out, nil
}

// memberView is a member with what its branch holds. Changes that cannot
// be read (its worktree is mid-rebase, say) are said in its status, never
// shown as none.
func (s *Service) memberView(m store.RunnerSession) Member {
	out := Member{Session: m, Repo: filepath.Base(m.Repo), Status: "Reading only"}
	if m.Worktree == "" {
		return out
	}
	trunk, err := s.Lander.Git.Trunk(m.Repo)
	if err == nil {
		var changes taskq.BranchChanges
		if changes, err = s.Changes(m.Worktree, trunk); err == nil {
			out.Changes, out.Status = &changes, changeStatus(changes)
			return out
		}
	}
	log.Printf("threads: #%d: changes of %s not read: %v", m.ThreadID, m.Name, err)
	out.Status = "Changes not read: " + firstLine(err.Error())
	return out
}

// changeStatus is one line on what a branch holds.
func changeStatus(c taskq.BranchChanges) string {
	switch {
	case c.Commits == 0 && c.Uncommitted == 0:
		return "No changes yet"
	case c.Uncommitted == 0:
		return plural(c.Commits, "commit") + ", all committed"
	case c.Commits == 0:
		return plural(c.Uncommitted, "file") + " not committed"
	}
	return plural(c.Commits, "commit") + ", " + plural(c.Uncommitted, "file") + " not committed"
}

func plural(n int, word string) string {
	if n == 1 {
		return "1 " + word
	}
	return strconv.Itoa(n) + " " + word + "s"
}
