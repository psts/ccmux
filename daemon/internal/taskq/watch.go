package taskq

import (
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

const (
	// StartGrace is how long a new pane may sit at a shell before Claude
	// is counted as gone: the command is typed into a shell and Claude
	// takes a moment to replace it.
	StartGrace = 2 * time.Minute
	// QuietLimit is how long a worker may sit finished without reporting
	// before the task asks for a person. A worker reports the moment it is
	// done, so a turn that ended this long ago without one ended unasked.
	QuietLimit = 10 * time.Minute
)

// Notes the watcher writes. The worker's own report notes replace them.
const (
	noteGone   = "its session closed before it reported"
	noteExited = "Claude exited before it reported"
	noteAsking = "waiting on a question or permission in its session"
	noteQuiet  = "stopped without reporting; open its session to see why"
	noteNoPane = "it lost its pane before starting"
)

// Judge reads a running or waiting-on-a-person task against its pane and
// returns the task as it should now be saved, and whether it changed. ok is
// false when no live session holds the pane any more.
func Judge(t store.Task, st PaneState, ok bool, now time.Time) (store.Task, bool) {
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

// judgeRunning moves a working task to "needs you" when its Claude asks
// something, or finished a turn long ago without reporting.
func judgeRunning(t store.Task, st PaneState, now time.Time) (store.Task, bool) {
	switch {
	case st.asking():
		t.Status, t.Note = NeedsYou, noteAsking
	case st.finished() && now.Sub(time.UnixMilli(st.At)) > QuietLimit:
		t.Status, t.Note = NeedsYou, noteQuiet
	default:
		return t, false
	}
	return t, true
}

// judgeWaitingOnYou puts a task back to running once a person dealt with
// it: they typed into its session, or answered the question it asked. Only
// a signal newer than the task's last change counts, so the Stop that ends a
// worker's own "stuck" report does not read as an answer.
func judgeWaitingOnYou(t store.Task, st PaneState) (store.Task, bool) {
	if st.At <= t.UpdatedAt {
		return t, false
	}
	answered := t.Note == noteAsking && !st.asking()
	if !st.prompted() && !answered {
		return t, false
	}
	t.Status, t.Note = Running, ""
	return t, true
}

func finish(t store.Task, status, note string, now time.Time) store.Task {
	t.Status, t.Note, t.FinishedAt = status, note, now.UnixMilli()
	return t
}
