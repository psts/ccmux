package taskq

import "ccmux.dev/ccmuxd/internal/store"

// Run is what a part's Claude is started with: Claude Code's --model and
// --effort. Without them a worker took whatever the person's own settings
// made the default, which had nothing to do with the task.
type Run struct{ Model, Effort string }

// NormalRun builds a part unless its planner called it hard. Most parts are
// small and come with a full plan: of the first 57, 48 were built in under
// 45 minutes, and the model that wrote the plan had done the thinking.
// HardRun is for the few that stay hard with the plan in hand. Both name an
// alias, not a model id, so each follows its family's newest model.
var (
	NormalRun = Run{Model: "opus", Effort: "medium"}
	HardRun   = Run{Model: "fable", Effort: "high"}
)

// WithRun returns p set to start as a hard part or a normal one.
func WithRun(p store.Part, hard bool) store.Part {
	run := NormalRun
	if hard {
		run = HardRun
	}
	p.Model, p.Effort = run.Model, run.Effort
	return p
}

// RunOf is what part p starts with: what it was filed with, or NormalRun
// for a part filed before parts had one.
func RunOf(p store.Part) Run {
	if p.Model == "" || p.Effort == "" {
		return NormalRun
	}
	return Run{Model: p.Model, Effort: p.Effort}
}
