package threads

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"

	"ccmux.dev/ccmuxd/internal/jobs"
	"ccmux.dev/ccmuxd/internal/runner"
	"ccmux.dev/ccmuxd/internal/store"
	"ccmux.dev/ccmuxd/internal/taskq"
)

// A thread's merge is a job (internal/jobs): the landing itself
// (taskq.RunLand: rebase, checks, fast-forward) runs in a service of its
// own, and this daemon only starts it and acts on how it ended. So a merge
// outlives a deploy and a crash of the daemon, and after a reboot it is run
// again and skips what it had finished.
//
// What the daemon does at the end (the thread's row, the word to the lead,
// taking the team apart) can be cut off too, and is then done again from
// the top. Every step of it is therefore recorded in the job's applied
// journal and skipped the second time, and the word to the lead carries an
// id made from the job's, so it cannot be said twice.

// Jobs is the job manager as a thread uses it (*jobs.Manager).
type Jobs interface {
	Start(jobs.New) (store.Job, error)
	Stop(id, note string) error
	OpenJob(subject string) (*store.Job, error)
	LastJob(subject string) (*store.Job, error)
	Unit(id string) string
	Steps(id string) ([]jobs.Entry, error)
	Result(id string) (jobs.Result, error)
	Applied(id string) (*jobs.Journal, error)
}

// MergeKind is the kind of a thread's merge job. Its Runner is the task
// queue's landing (taskq.RunLand), its Handler this package's
// (MergeHandler).
const MergeKind = "thread-merge"

const (
	// mergeLimit is how long one run of a merge may take: far past any real
	// one, short of the service's own backstop.
	mergeLimit = 3 * time.Hour
	// checkLine is how long one line of a repo's checks may run, when the
	// service does not say (Service.CheckLine).
	checkLine = 20 * time.Minute
)

// mergeSubject is whose job a thread's merge is.
func mergeSubject(threadID int64) string { return "thread:" + strconv.FormatInt(threadID, 10) }

// askFormat is the format of mergeAsk as a job's row keeps it. A daemon
// that finds another one does not read it (orphaned).
const askFormat = 1

// mergeAsk is what the daemon keeps of a merge it started, on the job's
// row: who asked, and the team as it was. The team's rows change while the
// daemon takes it apart, and a tidy-up that is run again still has to know
// which worktrees there were.
type mergeAsk struct {
	Format   int         `json:"format"`
	ThreadID int64       `json:"threadId"`
	By       string      `json:"by"`
	Members  []askMember `json:"members"` // the lead first
}

// askMember is one member as the merge found it.
type askMember struct {
	ID, Name, Role, Repo, Worktree, Branch string
}

func (m askMember) session(threadID int64) store.RunnerSession {
	return store.RunnerSession{ID: m.ID, Name: m.Name, Role: m.Role, Repo: m.Repo, Worktree: m.Worktree, Branch: m.Branch, ThreadID: threadID}
}

func (a mergeAsk) sessions() []store.RunnerSession {
	out := make([]store.RunnerSession, len(a.Members))
	for i, m := range a.Members {
		out[i] = m.session(a.ThreadID)
	}
	return out
}

// lead is the thread's lead as the merge found it.
func (a mergeAsk) lead() store.RunnerSession {
	for _, m := range a.sessions() {
		if m.Role == store.RoleLead {
			return m
		}
	}
	return store.RunnerSession{}
}

// startMerge marks the thread as merging and starts its job. A job that
// does not start leaves the thread open, with the reason.
func (s *Service) startMerge(th store.Thread, lead store.RunnerSession, members, work []store.RunnerSession, by string) error {
	ask := mergeAsk{Format: askFormat, ThreadID: th.ID, By: by}
	for _, m := range members {
		ask.Members = append(ask.Members, askMember{ID: m.ID, Name: m.Name, Role: m.Role, Repo: m.Repo, Worktree: m.Worktree, Branch: m.Branch})
	}
	perLine := s.CheckLine
	if perLine <= 0 {
		perLine = checkLine
	}
	th.State, th.Note, th.UpdatedAt = store.ThreadMerging, "", s.millis()
	if err := s.St.SaveThread(th); err != nil {
		return err
	}
	_, err := s.Jobs.Start(jobs.New{
		Kind: MergeKind, Subject: mergeSubject(th.ID), Name: fmt.Sprintf("merge-thread-%d", th.ID),
		Input: taskq.LandInput{Work: works(work), Tag: s.Lander.Tag, PerLineSeconds: int(perLine.Seconds())},
		Ctx:   ask, Limit: mergeLimit,
	})
	if err != nil {
		_ = s.settle(th, store.ThreadOpen, "the merge did not start: "+firstLine(err.Error())) // logged there; the start's own error is what is said
		return fmt.Errorf("thread #%d: the merge did not start: %w", th.ID, err)
	}
	s.changed(lead.ID)
	return nil
}

// changed tells whoever shows the thread that it looks different now. The
// lead is read fresh by its id: what a merge kept of it (mergeAsk) has no
// workspace, and whoever listens finds the thread's page by that.
func (s *Service) changed(leadID string) {
	if s.Changed == nil {
		return
	}
	lead, err := s.St.RunnerSession(leadID)
	if err != nil || lead == nil {
		log.Printf("threads: the lead %s was not read, so its page is not told to read the thread again: %v", leadID, err)
		return
	}
	s.Changed(*lead)
}

// Who stopped a merge, as its job's row says it.
const (
	stoppedByPerson = "stopped by a person on the thread's page"
	stoppedByLead   = "stopped by the thread's lead"
)

// StopMerge stops a thread's merge that is running: its service is told to
// stop, which ends its checks with it. The thread is open again, and its
// lead told, once the service is gone (mergeHandler.Stopped), a few seconds
// later. A trunk the merge had already moved stays moved, and the message
// says which.
//
// It takes no lock of the thread: the job's end is settled under the job's
// lock and then the thread's, in that order (jobs.Manager.lock).
func (s *Service) StopMerge(threadID int64, by string) (string, error) {
	if _, _, err := s.led(threadID); err != nil {
		return "", err
	}
	if s.Jobs == nil {
		return "", fmt.Errorf("thread #%d is not being merged", threadID)
	}
	job, err := s.Jobs.OpenJob(mergeSubject(threadID))
	if err != nil {
		return "", err
	}
	if job == nil {
		return "", fmt.Errorf("thread #%d is not being merged", threadID)
	}
	if job.State == store.JobStopping {
		return "", fmt.Errorf("thread #%d's merge is being stopped already", threadID)
	}
	note := stoppedByLead
	if by == ByPerson {
		note = stoppedByPerson
	}
	if err := s.Jobs.Stop(job.ID, note); err != nil {
		return "", fmt.Errorf("thread #%d's merge was not stopped: %w", threadID, err)
	}
	return fmt.Sprintf("Stopping the merge of thread #%d. Its checks end with it, in a few seconds; then the thread is open again and the lead gets a message that says what, if anything, had already landed.", threadID), nil
}

// MergeHandler is the daemon's side of a thread's merge job.
func (s *Service) MergeHandler() jobs.Handler { return mergeHandler{s} }

type mergeHandler struct{ s *Service }

// Apply acts on a merge's result. Failed: the thread is open again,
// nothing of the team is touched, and the lead is told why, with the member
// whose worktree the failure is in when it is one member's to fix. Landed:
// the helpers are stood down, every worktree goes (the lead is back in its
// repo's folder first, reading), and the lead is told.
func (h mergeHandler) Apply(job store.Job, res jobs.Result) error {
	s := h.s
	ask, err := readAsk(job)
	if err != nil {
		return s.orphaned(job, err)
	}
	out, err := taskq.ReadLandOutput(res)
	if err != nil {
		return s.orphaned(job, err)
	}
	end, err := s.ending(job, ask)
	if err != nil || end == nil {
		return err
	}
	defer end.unlock()
	if failure := out.Err(); failure != nil {
		log.Printf("threads: #%d merge failed: %v", ask.ThreadID, failure)
		return end.failed(failure, out.Landed)
	}
	log.Printf("threads: #%d %s", ask.ThreadID, out.Note)
	return end.landed(out.Note)
}

// failed settles a merge that failed: the thread is open again, nothing of
// the team is touched, and the lead is told why.
func (e *ending) failed(failure error, landed []string) error {
	note := "merge failed: " + firstLine(failure.Error())
	if e.gone {
		if len(landed) > 0 {
			note += " (already merged: " + strings.Join(landed, ", ") + ")"
		}
		return e.leadless(note)
	}
	if err := e.settle(store.ThreadOpen, note); err != nil {
		return err
	}
	return e.tell(asked(e.ask.By) + failedText(e.th, worked(e.ask.sessions()), failure, landed))
}

// landed settles a merge that landed: the team is taken apart, the thread
// reads as merged, and the lead is told.
func (e *ending) landed(note string) error {
	if e.gone {
		return e.leadless(note)
	}
	// Once, as a whole: a second settling of the same job must not take
	// apart what the thread has become since (a helper stood up for the
	// lead's next piece of work is not this merge's to end).
	left := once(e.steps, "dissolved", func() []string { return e.s.dissolve(e.ask, e.steps) })
	if err := e.settle(store.ThreadMerged, note+stayed(left)); err != nil {
		return err
	}
	e.s.reindex()
	return e.tell(asked(e.ask.By) + landedText(e.th, e.ask.lead(), note, left))
}

// Lost settles a merge that ended with no result the daemon reads. Nothing
// of the team is touched and the thread is open again; the lead is told
// what the job's journal says had landed, because a merge that died between
// two trunks is half in. A result that is there and cannot be read (it is
// another version's) is another matter: that merge may have landed whole,
// and nothing is said about it but that (orphaned).
func (h mergeHandler) Lost(job store.Job, cause error) error {
	if jobs.Unread(cause) {
		return h.s.orphaned(job, cause)
	}
	why := cause.Error()
	return h.cutShort(job, "merge did not finish: "+why, func(th store.Thread, landed string) string {
		return fmt.Sprintf("[ccmux] Thread #%d: its merge did not finish: %s. %s The thread stays open with its team as it was. "+
			"Its log is `journalctl --user -u %s`. Look at what it says before you ask for the merge again; what already landed is skipped by it.",
			th.ID, why, landed, h.s.Jobs.Unit(job.ID))
	})
}

// Stopped settles a merge that was stopped on request (StopMerge).
func (h mergeHandler) Stopped(job store.Job) error {
	who := "as you asked"
	if job.Note == stoppedByPerson {
		who = "by a person, on the thread's page"
	}
	return h.cutShort(job, "merge stopped", func(th store.Thread, landed string) string {
		return fmt.Sprintf("[ccmux] Thread #%d: its merge was stopped %s. %s The thread stays open with its team as it was; "+
			"ask for the merge again when you want it.", th.ID, who, landed)
	})
}

// cutShort settles a merge that left no result: the thread is open again
// with note, and the lead is told what text makes of it, given what the
// merge's journal says had landed.
func (h mergeHandler) cutShort(job store.Job, note string, text func(th store.Thread, landed string) string) error {
	s := h.s
	ask, err := readAsk(job)
	if err != nil {
		return s.orphaned(job, err)
	}
	end, err := s.ending(job, ask)
	if err != nil || end == nil {
		return err
	}
	defer end.unlock()
	steps, err := s.Jobs.Steps(job.ID)
	landed := "Nothing had landed."
	switch got := taskq.Landed(steps); {
	case err != nil:
		landed = "What had landed could not be read (" + err.Error() + "): look at each repo's trunk."
	case len(got) > 0:
		landed = "Already merged, and staying so: " + strings.Join(got, ", ") + "."
		note += " (already merged: " + strings.Join(got, ", ") + ")"
	}
	if end.gone {
		return end.leadless(note)
	}
	if err := end.settle(store.ThreadOpen, note); err != nil {
		return err
	}
	return end.tell(text(end.th, landed))
}

// ending is a merge job's end being settled: the thread's lock, its row,
// and the journal of what the daemon already did about this job.
type ending struct {
	s     *Service
	job   store.Job
	ask   mergeAsk
	th    store.Thread
	steps *jobs.Journal
	// gone is that the thread's lead has ended: the merge's end takes the
	// thread apart, and tells nobody (leadended.go).
	gone   bool
	unlock func()
}

// ending takes the thread's lock and reads what settling a job needs. A
// thread that is gone has nothing to settle: nil, and the job is done.
func (s *Service) ending(job store.Job, ask mergeAsk) (*ending, error) {
	unlock := s.lock(threadKey(ask.ThreadID))
	th, err := s.St.Thread(ask.ThreadID)
	if err != nil || th == nil {
		unlock()
		return nil, err
	}
	steps, err := s.Jobs.Applied(job.ID)
	if err != nil {
		unlock()
		return nil, fmt.Errorf("thread #%d: the record of what was done with its merge: %w", ask.ThreadID, err)
	}
	gone, err := s.leadGone(th.LeadSession)
	if err != nil {
		unlock()
		return nil, fmt.Errorf("thread #%d: %w", ask.ThreadID, err)
	}
	return &ending{s: s, job: job, ask: ask, th: *th, steps: steps, gone: gone, unlock: unlock}, nil
}

// settle records how the merge ended on the thread's row, once: a second
// settling of the same job (the daemon was cut off after the first) leaves
// a row alone that the lead's next change may have opened again since. A
// thread that is no longer merging (its lead ended meanwhile, and its
// thread with it) keeps what it says.
//
// A row that could not be written is an error and is not recorded as
// settled: the job stays open and is settled again, so the thread never
// keeps saying "merging" after its merge has ended.
func (e *ending) settle(state, note string) error {
	if e.steps.Done("settled", nil) {
		return nil
	}
	if e.th.State == store.ThreadMerging {
		if err := e.s.settle(e.th, state, note); err != nil {
			return fmt.Errorf("thread #%d: how its merge ended was not recorded: %w", e.th.ID, err)
		}
		e.th.State, e.th.Note = state, note
	}
	e.record("settled", nil)
	return nil
}

func (e *ending) record(key string, data any) {
	if err := e.steps.Record(key, data); err != nil {
		log.Printf("threads: #%d: step %s of settling its merge was not written down, so it may be done again: %v", e.ask.ThreadID, key, err)
	}
}

// tell sends the lead how the merge ended, under an id made from the
// job's: said again for the same job, it is queued once (runner SendAs).
// A lead that has ended is not an error; anything else is, and the job is
// settled again later.
func (e *ending) tell(text string) error {
	lead := e.ask.lead()
	e.s.changed(lead.ID)
	_, err := e.s.Sessions.SendAs(lead.ID, promptID(e.job.ID, "ended"), text, sender)
	if errors.Is(err, runner.ErrSessionDone) || errors.Is(err, store.ErrNoSession) {
		log.Printf("threads: #%d: its lead has ended and was not told: %s", e.ask.ThreadID, firstLine(text))
		return nil
	}
	if err != nil {
		return fmt.Errorf("thread #%d: its lead was not told: %w", e.ask.ThreadID, err)
	}
	return nil
}

// promptSpace is the namespace the ids of merge notices are made in.
var promptSpace = uuid.MustParse("6f0c1f3e-58a4-4d53-9f0b-2c3f1b7a9d41")

// promptID is the id of the notice about what of a job: the same for the
// same job and what, every time, and a UUID as a prompt's id must be.
func promptID(jobID, what string) string {
	return uuid.NewSHA1(promptSpace, []byte(jobID+":"+what)).String()
}

// readAsk reads what the daemon kept of a merge on its job's row.
func readAsk(job store.Job) (mergeAsk, error) {
	var ask mergeAsk
	if err := json.Unmarshal([]byte(job.Ctx), &ask); err != nil {
		return ask, fmt.Errorf("what was kept of the merge is not readable: %w", err)
	}
	if ask.Format != askFormat {
		return ask, fmt.Errorf("what was kept of the merge is in format %d and this ccmuxd reads format %d", ask.Format, askFormat)
	}
	return ask, nil
}

// orphaned settles a merge job this daemon cannot read (what it kept of
// it, or its result, is in another format: the job was started by another
// version). It guesses nothing: the thread is open again and says so, its
// lead is told to look at the trunks, and nothing of the team is touched.
func (s *Service) orphaned(job store.Job, cause error) error {
	log.Printf("threads: MERGE JOB %s NOT READ: %v; nothing is assumed about what landed", job.ID, cause)
	id, err := strconv.ParseInt(strings.TrimPrefix(job.Subject, "thread:"), 10, 64)
	if err != nil {
		return nil // not even a thread's: nothing to tell anyone
	}
	defer s.lock(threadKey(id))()
	th, err := s.St.Thread(id)
	if err != nil || th == nil {
		return err
	}
	note := "its merge ended and its outcome could not be read: " + firstLine(cause.Error())
	if th.State == store.ThreadMerging {
		if gone, err := s.leadGone(th.LeadSession); err != nil || gone {
			return s.orphanedEnd(*th, note, err)
		}
		if err := s.settle(*th, store.ThreadOpen, note); err != nil {
			return err
		}
	}
	text := fmt.Sprintf("[ccmux] Thread #%d: its merge ended, and this ccmuxd cannot read how (%v). It may have landed, or half of it. "+
		"Look at each repo's trunk and at `journalctl --user -u %s`, tell the person what you find, and ask for the merge again only then.", id, cause, s.Jobs.Unit(job.ID))
	_, err = s.Sessions.SendAs(th.LeadSession, promptID(job.ID, "ended"), text, sender)
	if errors.Is(err, runner.ErrSessionDone) || errors.Is(err, store.ErrNoSession) {
		return nil
	}
	return err
}

// Recover is the start of the daemon as a thread sees it. A thread whose
// merge job has not ended is left exactly as it is: the job manager watches
// the job, and settles it when it ends (MergeHandler), whether it ran
// through the restart or has to be run again. Only a thread that says it is
// merging and has no job at all is put back to open, with a word to its
// lead: its merge was never started, or was started by a daemon from before
// merges were jobs.
func (s *Service) Recover() {
	s.reindex()
	threads, err := s.St.Threads()
	if err != nil {
		log.Printf("threads: not read at start, a merge that never started stays marked as merging: %v", err)
		return
	}
	for _, th := range threads {
		if th.State == store.ThreadMerging && !s.hasMergeJob(th) {
			s.cutOff(th)
		}
	}
}

// hasMergeJob reports whether a merge job of the thread has not ended. When
// that cannot be asked, the answer is yes: a merging thread is only ever
// reset on a plain no.
func (s *Service) hasMergeJob(th store.Thread) bool {
	if s.Jobs == nil {
		return false
	}
	job, err := s.Jobs.OpenJob(mergeSubject(th.ID))
	if err != nil {
		log.Printf("threads: #%d: its merge job was not read, so it stays as it is: %v", th.ID, err)
		return true
	}
	return job != nil
}

// cutOff opens a thread whose merge has no job, and tells its lead once:
// the notice is named after the moment the merge was asked for.
func (s *Service) cutOff(th store.Thread) {
	asked := th.UpdatedAt
	if err := s.settle(th, store.ThreadOpen, "its merge was cut off by a daemon restart"); err != nil {
		return // still merging on its row: the daemon's next start finds it again, and tells its lead then
	}
	text := fmt.Sprintf("[ccmux] Thread #%d: its merge was cut off by a daemon restart, so it may be half done. "+
		"Ask for the merge again: what already landed is skipped.", th.ID)
	id := promptID(fmt.Sprintf("thread-%d-merge-%d", th.ID, asked), "cut-off")
	if _, err := s.Sessions.SendAs(th.LeadSession, id, text, sender); err != nil {
		log.Printf("threads: #%d: its lead was not told (%v): %s", th.ID, err, firstLine(text))
	}
}
