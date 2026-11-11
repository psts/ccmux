package taskq

import (
	"fmt"
	"time"

	"ccmux.dev/ccmuxd/internal/model"
	"ccmux.dev/ccmuxd/internal/store"
)

// PaneState is what the runner reads off a worker's pane: the last hook
// signal its Claude sent (never retired by a person looking, unlike the
// pane's attention), and whether the pane is back at a shell.
type PaneState struct {
	Attention model.Attention
	Reason    model.AttentionReason
	At        int64 // when that signal arrived, unix ms; 0 for none yet
	AtShell   bool
}

// asking is a question or a permission prompt: blocked on a person.
func (st PaneState) asking() bool {
	return st.Attention == model.AttentionNeedsInput && st.Reason != model.ReasonFinished
}

// finished is a turn that ended: a Stop, or Claude's idle reminder after one.
func (st PaneState) finished() bool {
	return st.Attention == model.AttentionDone ||
		(st.Attention == model.AttentionNeedsInput && st.Reason == model.ReasonFinished)
}

// prompted is a prompt submitted to it: a person typed into the session.
func (st PaneState) prompted() bool { return st.Attention == model.AttentionIdle && st.At != 0 }

// reminded is Claude's idle reminder, sent a minute after a turn really
// ended. A Stop alone does not say that: another Stop hook (a receipt to
// sign) can send the worker straight back to work, with no sign of it here.
func (st PaneState) reminded() bool {
	return st.Attention == model.AttentionNeedsInput && st.Reason == model.ReasonFinished
}

const (
	// StartGrace is how long a new pane may sit at a shell before Claude
	// is counted as gone: the command is typed into a shell and Claude
	// takes a moment to replace it.
	StartGrace = 2 * time.Minute
	// QuietLimit is how long a worker may sit finished without reporting
	// before the runner asks it to report, and after that once more before
	// the task asks for a person. A worker reports the moment it is done,
	// so a turn that ended this long ago without one ended unasked.
	QuietLimit = 10 * time.Minute
	// SilentLimit is how long a worker may give no sign at all before the
	// part asks for a person. Its first prompt fires a hook within seconds,
	// so silence this long means it is stuck on a screen nothing answers,
	// or the daemon restarted and forgot what it last heard.
	SilentLimit = 10 * time.Minute
	// NudgeLimit is how long a worker asked to report (Runner.nudge) may
	// give no sign at all before the part asks for a person. The message
	// counts as a prompt, and a prompted worker is otherwise never timed:
	// a long turn sends nothing until it ends. So this has to outlast the
	// longest thing the message can start, a full run of the repo's checks
	// (one line may take twenty minutes). Silence past it means the
	// message never reached a Claude, and without a limit the part would
	// read as at work for good, holding its slot. Only the nudge's own
	// prompt is timed: once a person types into the session, or a turn
	// ends, the worker is alive and this no longer applies.
	NudgeLimit = 3 * QuietLimit
	// SettleLimit is how long a merge waits for a built part's worker to
	// end its turn when no idle reminder comes (a hook dropped, subagents
	// still counted as running). A worker sent back to work by its Stop
	// hook is running the repo's checks once more: three to nine minutes
	// in the two repos measured. A run longer than this is merged under,
	// as every run was before the wait existed.
	SettleLimit = 10 * time.Minute
)

// Notes the watcher writes. The worker's own report notes replace them.
const (
	noteGone   = "its session closed before it reported"
	noteExited = "Claude exited before it reported"
	noteAsking = "waiting on a question or permission in its session"
	noteQuiet  = "stopped without reporting; open its session to see why"
	noteNoPane = "it lost its pane before starting"
	noteSilent = "no sign of its Claude working; open its session to see why"
	// noteUnanswered: the runner asked a quiet worker to report and heard
	// nothing for NudgeLimit.
	noteUnanswered = "stopped without reporting, and gave no sign when asked to; open its session to see why"
)

// noteNudged is a working part's note once the runner has asked its quiet
// worker to report (Runner.nudge). The worker's report, or a send-back,
// replaces it.
const noteNudged = "stopped without reporting; ccmux asked it to report"

// nudgeText is what a worker reads when it ended its turn long ago without
// reporting. Most such workers are waiting for something to wake them that
// never will: a commit review that found nothing sends no message.
func nudgeText(taskID int64) string {
	return fmt.Sprintf(`[ccmux] Task #%d: you ended your turn a while ago without reporting, and
nothing is going to wake you: a clean review sends no message, and nobody
is watching this session. Do one of these now:

- Your part is done, committed and its checks are green: call the tasks
  tool with action "report", status "done".
- You were waiting for something (a review, an install, a test run): look
  at it yourself now, finish, then report.
- You cannot go on: report "stuck" with exactly what you need.

This is the only reminder. If you stop again without a report, the task
waits for a person.`, taskID)
}

// noteSettling is a built feature's note while its merge waits for a
// worker's turn to end (Settled).
const noteSettling = "waiting for its worker to end its turn before the checks run"

// Judge reads a running or waiting-on-a-person part against its pane and
// returns the part as it should now be saved, and whether it changed. ok is
// false when no live session holds the pane any more.
func Judge(t store.Part, st PaneState, ok bool, now time.Time) (store.Part, bool) {
	if t.Status != Running && t.Status != NeedsYou {
		return t, false
	}
	switch {
	case t.PaneID == "":
		return finish(t, Failed, noteNoPane, now), true
	case !ok:
		return finish(t, Failed, noteGone, now), true
	case st.AtShell && now.Sub(time.UnixMilli(t.StartedAt)) > StartGrace:
		return finish(t, Failed, noteExited, now), true
	}
	if t.Status == Running {
		return judgeRunning(t, st, now)
	}
	return judgeWaitingOnYou(t, st)
}

// judgeRunning moves a working part to "needs you" when its Claude asks
// something, finished a turn long ago without reporting, never gave a sign,
// or was asked to report and gave none since.
func judgeRunning(t store.Part, st PaneState, now time.Time) (store.Part, bool) {
	switch {
	case st.asking():
		t.Status, t.Note = NeedsYou, noteAsking
	case st.finished() && now.Sub(time.UnixMilli(st.At)) > QuietLimit:
		t.Status, t.Note = NeedsYou, noteQuiet
	case st.At == 0 && now.Sub(time.UnixMilli(t.StartedAt)) > SilentLimit:
		t.Status, t.Note = NeedsYou, noteSilent
	case unanswered(t, st) && now.Sub(time.UnixMilli(st.At)) > NudgeLimit:
		t.Status, t.Note = NeedsYou, noteUnanswered
	default:
		return t, false
	}
	return t, true
}

// unanswered reports whether a part asked to report has shown nothing
// since: its pane's last sign is still the nudge's own prompt, which was
// noted just before the part was saved as asked. Any later sign (a person
// typing, a turn ending) is newer than that save.
func unanswered(t store.Part, st PaneState) bool {
	return t.Note == noteNudged && st.prompted() && st.At <= t.UpdatedAt
}

// judgeWaitingOnYou puts a part back to running once a person dealt with
// it: they typed into its session, or answered the question it asked. Only
// a signal newer than the part's last change counts, so the Stop that ends a
// worker's own "stuck" report does not read as an answer.
func judgeWaitingOnYou(t store.Part, st PaneState) (store.Part, bool) {
	if st.At <= t.UpdatedAt {
		return t, false
	}
	// Asking or silent, any newer sign that is not another question means
	// it moved on; after its own "stuck" report only a person typing does.
	answered := (t.Note == noteAsking || t.Note == noteSilent) && !st.asking()
	if !st.prompted() && !answered {
		return t, false
	}
	t.Status, t.Note = Running, ""
	return t, true
}

// Settled reports whether a built part's worker has ended the turn it
// reported in, so a merge may run the repo's checks in its worktree: the
// idle reminder came since the report, or its Claude is gone, or it gave no
// sign for SettleLimit. Until then the worker may be running the same
// checks there itself, and two runs at once fail each other on tests that
// only lose to a busy machine. ok is false when no live session holds the
// pane.
func Settled(p store.Part, st PaneState, ok bool, now time.Time) bool {
	if p.PaneID == "" || !ok || st.AtShell {
		return true
	}
	if st.reminded() && st.At >= p.FinishedAt {
		return true
	}
	last := p.FinishedAt
	if st.At > last {
		last = st.At
	}
	return now.Sub(time.UnixMilli(last)) > SettleLimit
}

func finish(t store.Part, status, note string, now time.Time) store.Part {
	t.Status, t.Note, t.FinishedAt = status, note, now.UnixMilli()
	return t
}
