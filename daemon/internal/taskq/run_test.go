package taskq

import (
	"strings"
	"testing"

	"ccmux.dev/ccmuxd/internal/store"
)

// A part is filed normal unless its planner calls it hard, and a part from
// before parts had a model starts as a normal one.
func TestRun_NormalUnlessHard(t *testing.T) {
	normal, hard := WithRun(store.Part{Plan: "p"}, false), WithRun(store.Part{Plan: "p"}, true)
	if RunOf(normal) != NormalRun || RunOf(hard) != HardRun || NormalRun == HardRun {
		t.Fatalf("normal = %+v, hard = %+v", RunOf(normal), RunOf(hard))
	}
	if normal.Plan != "p" || hard.Model != HardRun.Model || hard.Effort != HardRun.Effort {
		t.Fatalf("WithRun changed more than the run: %+v %+v", normal, hard)
	}
	if got := RunOf(store.Part{}); got != NormalRun {
		t.Fatalf("a part filed with no model starts with %+v, want the normal run", got)
	}
	if got := RunOf(store.Part{Model: "fable"}); got != NormalRun {
		t.Fatalf("half a run = %+v, want the normal run", got)
	}
}

// What a part is built with changes like its plan: only before the task starts.
func TestRun_EditOnlyBeforeItStarts(t *testing.T) {
	p := WithRun(store.Part{ID: 1}, false)
	next, err := EditRun(store.Task{ID: 1, Status: Waiting}, p, true)
	if err != nil || RunOf(next) != HardRun {
		t.Fatalf("a waiting task's part = %+v, %v", next, err)
	}
	back, err := EditRun(store.Task{ID: 1, Status: Waiting}, next, false)
	if err != nil || RunOf(back) != NormalRun {
		t.Fatalf("back to normal = %+v, %v", back, err)
	}
	for _, status := range []string{Running, NeedsYou, ReadyToMerge, Done} {
		got, err := EditRun(store.Task{ID: 1, Status: status}, p, true)
		if err == nil || got != p {
			t.Errorf("a %s task's part was changed: %+v, %v", status, got, err)
		}
	}
}

// The model and effort are flags of claude itself: after the "--" they
// would be read as part of the prompt.
func TestLaunchCommand_StartsOnThePartsRun(t *testing.T) {
	cmd := launchCommand(layout{dir: "/w/app-1", brief: "/w/app-1/task-7.md", busName: "app-task-7"}, HardRun)
	flags, prompt, found := strings.Cut(cmd, " -- ")
	if !found || !strings.Contains(prompt, "/w/app-1/task-7.md") {
		t.Fatalf("launch = %s", cmd)
	}
	for _, want := range []string{"--model 'fable'", "--effort 'high'", "--permission-mode auto", "CLAUDE_PEERS_NAME='app-task-7'", "--add-dir '/w/app-1'"} {
		if !strings.Contains(flags, want) {
			t.Errorf("launch flags = %s, want %s among them", flags, want)
		}
	}
}
