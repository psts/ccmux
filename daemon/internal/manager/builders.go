// Task builders as the manager sees them. Since task #176 the Claude that
// builds one part of a queued task is a runner session with the role
// builder (store/builders.go), not a Claude typed into a tmux pane. Its
// pane carries the task worker's stamp, so the three places that tell a
// worker's pane from a person's keep working for it: its signals to the
// queue's watchdog (tasks.go), the ports its own dev servers hold
// (listeners.go), and the clearing of its worktree before the next run.
//
// What differs from a tmux worker is here: what its runner's signals mean
// to the watchdog, when its Claude counts as gone, and how it is ended.
package manager

import (
	"errors"
	"fmt"

	"ccmux.dev/ccmuxd/internal/model"
	"ccmux.dev/ccmuxd/internal/store"
)

// runnerStamp is the createdBy of a runner session's pane: a task worker's
// for a builder, "runner" for every other session.
func runnerStamp(sess store.RunnerSession) string {
	if sess.Role == store.RoleBuilder {
		return taskRunner
	}
	return "runner"
}

// builderSignal turns what a builder's runner said (api/agentsignal.go)
// into the signal the queue's watchdog reads (taskq.PaneState), given the
// last one kept; false keeps the last one. The watchdog was written for a
// tmux worker's hooks, where a Stop is "finished", a prompt typed by a
// person is idle, and nothing at all is said while a turn runs. A runner
// says two things those hooks never do:
//
//   - busy (running), a second after any prompt reached it. A person's
//     prompt, or the queue's own message, was marked just before
//     (NoteTaskPrompt) and that mark must stand for the turn: it is what
//     puts a part that reported "stuck" back to work, and the watchdog
//     looks only every few seconds. So running never replaces that mark.
//     After anything else it is kept: the builder is at work.
//   - stopped (idle), when its runner was stopped mid-turn: a restart, or
//     put to sleep. To the watchdog idle means "a person typed", which this
//     is not. The turn is over, cut off, so it reads as a turn that ended:
//     a builder that then stays quiet is asked to report, like any other.
func builderSignal(last TaskSignal, att model.Attention, reason model.AttentionReason) (TaskSignal, bool) {
	switch att {
	case model.AttentionRunning:
		if last.Attention == model.AttentionIdle && last.At != 0 {
			return last, false
		}
	case model.AttentionIdle:
		return TaskSignal{Attention: model.AttentionDone, Reason: model.ReasonFinished}, true
	}
	return TaskSignal{Attention: att, Reason: reason}, true
}

// builderGone reports whether a builder session's Claude is gone for the
// queue's purposes, the twin of a tmux worker's pane back at a shell: its
// row says asleep. The supervisor put it to sleep after it stopped too
// often, or a person did. Read from the row as the pane mirrors it, never
// from whether its unit is up this second: a unit between two restarts (a
// planned one, a crash systemd is about to recover) is not a Claude that
// exited, and taking it for one would fail a part that is still building.
func builderGone(p *model.Pane) bool { return p.Status != model.StatusLive }

// endBuildersIn ends every builder session that works in dir. They are
// read from the session rows, not from the panes: a builder whose
// workspace was deleted under it has no pane and still holds the worktree,
// and one that stayed would refuse the next run its place in the task's
// thread. A session that does not stop is an error: the caller must not
// start a second Claude beside it, or remove the worktree under it.
func (m *Manager) endBuildersIn(dir string) error {
	if m.store == nil {
		return nil
	}
	builders, err := m.store.BuildersIn(dir)
	if err != nil {
		return fmt.Errorf("reading the builders in %s: %w", dir, err)
	}
	for _, b := range builders {
		if m.EndRunnerSession == nil {
			return errors.New("a builder session works there and this daemon runs no session supervisor to end it")
		}
		if err := m.EndRunnerSession(b.ID); err != nil {
			return fmt.Errorf("closing the last run's session: %w", err)
		}
	}
	return nil
}
