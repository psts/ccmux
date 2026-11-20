package threads

import (
	"fmt"
	"log"
	"path/filepath"
	"strconv"
	"strings"

	"ccmux.dev/ccmuxd/internal/jobs"
	"ccmux.dev/ccmuxd/internal/store"
	"ccmux.dev/ccmuxd/internal/taskq"
)

// Detail is a thread as its page shows it: its row, and each member with
// what its branch holds.
type Detail struct {
	Thread  store.Thread `json:"thread"`
	Members []Member     `json:"members"`
	// Merge is the thread's merge job: the one that runs, or the last one
	// when it did not land and the thread still says so. Nil otherwise.
	Merge *MergeView `json:"merge,omitempty"`
}

// MergeView is a thread's merge as its page shows it. State is "running",
// "stopping" (a stop was asked for), "failed" (it ran and did not land),
// "lost" (it ended with no result) or "stopped". Doing is the step a
// running merge is in the middle of ("backend: checks running"). CheckLog
// is the file with a failed check's whole output. Unit is the service whose
// log says the rest: journalctl --user -u <Unit>.
type MergeView struct {
	Job       string `json:"job"`
	Unit      string `json:"unit"`
	State     string `json:"state"`
	Doing     string `json:"doing,omitempty"`
	StartedAt int64  `json:"startedAt"`
	Attempts  int    `json:"attempts"`
	CheckLog  string `json:"checkLog,omitempty"`
}

// Running reports whether the merge has not ended.
func (m MergeView) Running() bool { return m.State == "running" || m.State == "stopping" }

// line is a running merge in words, for the lead.
func (m MergeView) line() string {
	doing := m.Doing
	if doing == "" {
		doing = "between steps"
	}
	if m.State == "stopping" {
		doing = "being stopped"
	}
	return fmt.Sprintf("%s (job %s, try %d; its log: journalctl --user -u %s)", doing, m.Job, m.Attempts, m.Unit)
}

// mergeView is the thread's merge job as its page shows it, nil for none.
// A merge that has ended is shown only while the thread still stands where
// it left it: open, with the note that merge wrote.
func (s *Service) mergeView(th store.Thread) *MergeView {
	if s.Jobs == nil {
		return nil
	}
	job, err := s.Jobs.LastJob(mergeSubject(th.ID))
	if err != nil {
		log.Printf("threads: #%d: its merge job was not read, so its page shows none: %v", th.ID, err)
		return nil
	}
	if job == nil {
		return nil
	}
	v := &MergeView{Job: job.ID, Unit: s.Jobs.Unit(job.ID), StartedAt: job.StartedAt, Attempts: job.Attempts}
	switch {
	case job.State == store.JobStopping:
		v.State = "stopping"
	case job.State != store.JobDone:
		v.State = "running"
		if steps, err := s.Jobs.Steps(job.ID); err == nil {
			v.Doing = jobs.Doing(steps)
		}
	case th.State != store.ThreadOpen || !strings.HasPrefix(th.Note, "merge "):
		return nil
	default:
		v.State, v.CheckLog = s.endedMerge(*job)
	}
	return v
}

// endedMerge is how a merge that did not land ended, and the file with its
// failed check's output when it has one.
func (s *Service) endedMerge(job store.Job) (state, checkLog string) {
	if job.Outcome != store.JobApplied {
		return job.Outcome, ""
	}
	res, err := s.Jobs.Result(job.ID)
	if err != nil {
		log.Printf("threads: job %s: its result was not read, so its page names no failed check: %v", job.ID, err)
		return "failed", ""
	}
	out, err := taskq.ReadLandOutput(res)
	if err != nil {
		log.Printf("threads: job %s: its result was not understood, so its page names no failed check: %v", job.ID, err)
	}
	return "failed", out.CheckLog
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
	out := Detail{Thread: *th, Members: make([]Member, len(members)), Merge: s.mergeView(*th)}
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
