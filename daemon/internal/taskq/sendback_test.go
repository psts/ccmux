package taskq

import (
	"errors"
	"strings"
	"testing"
	"time"

	"ccmux.dev/ccmuxd/internal/store"
)

// A red check goes back to that part's worker on its own, once: the part
// runs again in the same pane while its teammate stays built, nothing
// merges, and the worker reads why and that it may rebase. Fixed and
// reported again, the feature merges.
func TestSendBack_RedCheckGoesToItsWorkerOnce(t *testing.T) {
	g := newRig(t)
	g.feature(1, 0, "app", "backend")
	g.tick()
	g.doneParts(1)
	g.checks.results["/p/backend"] = CheckResult{Configured: true, Output: "FAIL: TestExport"}
	g.tick()
	task, app, backend := g.st.task(1), g.partsOf(1)[0], g.partsOf(1)[1]
	if task.Status != Running || task.AutoFixes != 1 || !strings.Contains(task.Note, "TestExport") {
		t.Fatalf("task after a red check = %+v", task)
	}
	if backend.Status != Running || backend.PaneID == "" || backend.StartedAt != g.now.UnixMilli() {
		t.Fatalf("red part = %+v, want running again in its pane from now", backend)
	}
	if app.Status != Ready || len(g.host.told[app.PaneID]) != 0 {
		t.Fatalf("the green teammate = %+v, told %v; want it left built and untold", app, g.host.told[app.PaneID])
	}
	told := g.host.told[backend.PaneID]
	if len(told) != 1 || !strings.Contains(told[0], "TestExport") || !strings.Contains(told[0], "rebase onto dev") {
		t.Fatalf("the worker was told %q", told)
	}
	if len(g.git.ffs) != 0 {
		t.Fatalf("a trunk moved: %v", g.git.ffs)
	}

	// Reported done, still red: the second failure is for a person.
	g.doneParts(1)
	g.tick()
	g.tick()
	if got := g.st.task(1); got.Status != MergeFailed || len(g.host.told[backend.PaneID]) != 1 {
		t.Fatalf("second failure = %+v, told %d times", got, len(g.host.told[backend.PaneID]))
	}

	// A person sends it back; the fix lands and the feature merges.
	if err := g.r.SendBackPart(1, backend.ID); err != nil {
		t.Fatal(err)
	}
	if got := g.st.part(backend.ID); got.Status != Running || len(g.host.told[backend.PaneID]) != 2 {
		t.Fatalf("after Send back = %+v", got)
	}
	delete(g.checks.results, "/p/backend")
	g.doneParts(1)
	g.tick()
	g.tick()
	if got := g.st.task(1); got.Status != Done {
		t.Fatalf("after the fix = %+v", got)
	}
}

// A worker nobody can reach keeps the failure for a person, says so, and
// spends the automatic try.
func TestSendBack_DeafWorkerStopsForAPerson(t *testing.T) {
	g := newRig(t)
	g.feature(1, 0, "app")
	g.tick()
	g.doneParts(1)
	g.host.deaf = true
	g.git.clash["/p/app"] = true
	g.tick()
	got := g.st.task(1)
	if got.Status != MergeFailed || got.AutoFixes != 1 || !strings.Contains(got.Note, "send-back to its worker failed") {
		t.Fatalf("deaf worker = %+v", got)
	}
	if p := g.st.part(g.partsOf(1)[0].ID); p.Status != Ready {
		t.Fatalf("its part = %s, want still built", p.Status)
	}
}

// Failures a worker cannot fix in its own worktree never go to it.
func TestSendBack_GateFilesStayWithAPerson(t *testing.T) {
	g := newRig(t)
	g.feature(1, 0, "app")
	g.tick()
	g.doneParts(1)
	g.git.gateChanges["/p/app"] = []string{".claude/qa-gates"}
	g.tick()
	if got := g.st.task(1); got.Status != MergeFailed || got.AutoFixes != 0 || len(g.host.told) != 0 {
		t.Fatalf("gate files = %+v, told %v", got, g.host.told)
	}
}

// The rule: only a built part of a feature that failed to merge, with a
// pane, goes back; anything else is refused and left as it was.
func TestSendBack_Rule(t *testing.T) {
	now := time.UnixMilli(5000)
	failed := store.Task{ID: 1, Status: MergeFailed, FinishedAt: 9}
	built := store.Part{ID: 2, Status: Ready, PaneID: "pane-1", Note: "done", FinishedAt: 9}
	nt, np, err := SendBack(failed, built, now)
	if err != nil || nt.Status != Running || nt.FinishedAt != 0 ||
		np.Status != Running || np.StartedAt != 5000 || np.FinishedAt != 0 || np.PaneID != "pane-1" {
		t.Fatalf("SendBack = %+v %+v %v", nt, np, err)
	}
	for name, c := range map[string]struct {
		t store.Task
		p store.Part
	}{
		"task still running": {store.Task{Status: Running}, built},
		"task merged":        {store.Task{Status: Done}, built},
		"part not built":     {failed, store.Part{Status: Failed, PaneID: "pane-1"}},
		"no worker pane":     {failed, store.Part{Status: Ready}},
	} {
		gt, gp, err := SendBack(c.t, c.p, now)
		if err == nil || gt != c.t || gp != c.p {
			t.Errorf("%s: SendBack = %+v %+v %v, want refused and unchanged", name, gt, gp, err)
		}
	}
}

// A send-back whose save fails says so: the worker was told, but its next
// report would bounce off a part still built, so "ok" would be a lie. The
// feature stays failed and its other part untouched.
func TestSendBack_UnsavedIsAnError(t *testing.T) {
	g := newRig(t)
	g.feature(1, 0, "app", "backend")
	g.tick()
	g.doneParts(1)
	failed := g.st.task(1)
	failed.Status = MergeFailed
	g.st.SaveTask(failed)
	app, backend := g.partsOf(1)[0], g.partsOf(1)[1]
	g.st.partErr = errors.New("database is locked")
	err := g.r.SendBackPart(1, backend.ID)
	if err == nil || !strings.Contains(err.Error(), "nothing was saved") {
		t.Fatalf("SendBackPart with a failing save = %v, want an error", err)
	}
	g.st.partErr = nil
	if got := g.st.task(1); got.Status != MergeFailed {
		t.Fatalf("task = %s, want still merge_failed", got.Status)
	}
	if got := g.st.part(backend.ID); got.Status != Ready {
		t.Fatalf("sent part = %s, want still built", got.Status)
	}
	if got := g.st.part(app.ID); got != app {
		t.Fatalf("the other part changed: %+v", got)
	}
}

// Every failure inside a part's own worktree goes back to that part's
// worker on its own; a repo with no checks never does, its green unproven
// by anything a worker could fix.
func TestSendBack_EachWorkerFixableKind(t *testing.T) {
	cases := []struct {
		name   string
		set    func(g *rig)
		toWork bool
	}{
		{"red checks", func(g *rig) { g.checks.results["/p/backend"] = CheckResult{Configured: true, Output: "FAIL"} }, true},
		{"clash with the trunk", func(g *rig) { g.git.clash["/p/backend"] = true }, true},
		{"uncommitted in its worktree", func(g *rig) { g.git.dirty["/p/backend"] = "?? x.go" }, true},
		{"worktree moved during the checks", func(g *rig) {
			g.checks.during = func() {
				g.git.mu.Lock()
				g.git.moved["/p/backend"] = "a new commit"
				g.git.mu.Unlock()
			}
		}, true},
		{"no checks listed", func(g *rig) { g.checks.results["/p/backend"] = CheckResult{} }, false},
	}
	for _, c := range cases {
		g := newRig(t)
		g.feature(1, 0, "app", "backend")
		g.tick()
		g.doneParts(1)
		c.set(g)
		g.tick()
		backend := g.partsOf(1)[1]
		task := g.st.task(1)
		sent := task.Status == Running && task.AutoFixes == 1 && backend.Status == Running &&
			len(g.host.told[backend.PaneID]) == 1
		if sent != c.toWork {
			t.Errorf("%s: task %s (auto fixes %d), part %s, told %d; want sent back = %v",
				c.name, task.Status, task.AutoFixes, backend.Status, len(g.host.told[backend.PaneID]), c.toWork)
		}
		if app := g.partsOf(1)[0]; len(g.host.told[app.PaneID]) != 0 {
			t.Errorf("%s: the green teammate was told", c.name)
		}
	}
}

// With the window's one slot taken (#2 started while #1 merged), #1's red
// check waits instead of going straight back to its worker: it stays merge
// failed, says it waits for a slot, and its worker hears nothing yet. Once
// #2 stops building, the send-back goes out, ahead of waiting #3.
func TestSendBack_WaitsForAFreeSlot(t *testing.T) {
	g := newRig(t)
	g.r.Slots = 1
	g.feature(1, 0, "app")
	g.feature(2, 0, "backend")
	g.feature(3, 0, "admin")
	g.tick() // #1 builds
	g.doneParts(1)
	g.checks.results["/p/app"] = CheckResult{Configured: true, Output: "FAIL: TestRoute"}
	g.tick() // #1 merges and fails; #2 took the slot
	one, app := g.st.task(1), g.partsOf(1)[0]
	if g.st.task(2).Status != Running {
		t.Fatalf("#2 = %s, want running in the slot #1 left", g.st.task(2).Status)
	}
	if one.Status != MergeFailed || one.FixPart != app.ID || !strings.Contains(one.Note, "waiting for a free slot") {
		t.Fatalf("#1 with no slot = %s fix %d note %q", one.Status, one.FixPart, one.Note)
	}
	if len(g.host.told[app.PaneID]) != 0 || app.Status != Ready {
		t.Fatalf("worker told %v, part %s; want untold and still built", g.host.told[app.PaneID], app.Status)
	}

	g.doneParts(2)
	g.tick() // #2 leaves its slot: #1 goes back to its worker, #3 waits
	one, app = g.st.task(1), g.partsOf(1)[0]
	if one.Status != Running || one.FixPart != 0 || app.Status != Running {
		t.Fatalf("#1 once a slot freed = %s fix %d part %s", one.Status, one.FixPart, app.Status)
	}
	told := g.host.told[app.PaneID]
	if len(told) != 1 || !strings.Contains(told[0], "TestRoute") || strings.Contains(told[0], "waiting for a free slot") {
		t.Fatalf("worker was told %q", told)
	}
	if g.st.task(3).Status != Waiting {
		t.Fatalf("#3 = %s, want waiting behind the send-back", g.st.task(3).Status)
	}
}

// A person's "Send back to worker" on a failed merge still waiting for a
// slot sends it now and spends the wait: the worker reads why without the
// waiting line, and nothing goes back a second time on a later pass.
func TestSendBack_PersonSendsAWaitingFixNow(t *testing.T) {
	g := newRig(t)
	g.r.Slots = 1
	g.feature(1, 0, "app")
	g.feature(2, 0, "backend")
	g.tick()
	g.doneParts(1)
	g.checks.results["/p/app"] = CheckResult{Configured: true, Output: "FAIL: TestRoute"}
	g.tick()
	app := g.partsOf(1)[0]
	if g.st.task(1).FixPart != app.ID {
		t.Fatalf("setup: #1 is not waiting to send back: %+v", g.st.task(1))
	}
	if err := g.r.SendBackPart(1, app.ID); err != nil {
		t.Fatal(err)
	}
	g.doneParts(2)
	g.tick()
	told := g.host.told[app.PaneID]
	if g.st.task(1).FixPart != 0 || len(told) != 1 || strings.Contains(told[0], "waiting for a free slot") {
		t.Fatalf("fix %d, told %q; want the wait spent and one clean send-back", g.st.task(1).FixPart, told)
	}
}

// waitingFix leaves #1 merge failed with its send-back waiting for the one
// slot #2 holds, and returns #1's part.
func waitingFix(t *testing.T, g *rig) store.Part {
	t.Helper()
	g.r.Slots = 1
	g.feature(1, 0, "app")
	g.feature(2, 0, "backend")
	g.tick()
	g.doneParts(1)
	g.checks.results["/p/app"] = CheckResult{Configured: true, Output: "FAIL: TestRoute"}
	g.tick()
	app := g.partsOf(1)[0]
	if g.st.task(1).FixPart != app.ID {
		t.Fatalf("setup: #1 is not waiting to send back: %+v", g.st.task(1))
	}
	return app
}

// A person tries the merge again while the send-back waits, and it fails
// again: that second failure is a person's (the one automatic try is
// spent), so nothing goes to the worker, now or when a slot frees.
func TestSendBack_NewFailureSpendsAWaitingFix(t *testing.T) {
	g := newRig(t)
	app := waitingFix(t, g)
	one := g.st.task(1)
	one.Status = ReadyToMerge
	g.st.SaveTask(one)
	g.tick()
	if got := g.st.task(1); got.Status != MergeFailed || got.FixPart != 0 {
		t.Fatalf("after a second failure = %s fix %d, want merge failed and no wait", got.Status, got.FixPart)
	}
	g.doneParts(2)
	g.tick()
	if told := g.host.told[app.PaneID]; len(told) != 0 {
		t.Fatalf("the worker was told %q after a person's failure", told)
	}
}

// Unreadable window numbers hold a waiting send-back (no panic, no
// message, the wait kept); a window paused at 0 holds it too; a window
// with room sends it.
func TestSendBack_WaitingFixFollowsTheWindowsNumber(t *testing.T) {
	g := newRig(t)
	app := waitingFix(t, g)
	g.doneParts(2) // #2 leaves its slot
	g.r.WindowSlots = func() (map[string]int, error) { return nil, errors.New("database is locked") }
	g.tick()
	if g.st.task(1).FixPart != app.ID || len(g.host.told[app.PaneID]) != 0 {
		t.Fatalf("unreadable numbers: fix %d told %v; want held", g.st.task(1).FixPart, g.host.told[app.PaneID])
	}
	g.r.WindowSlots = func() (map[string]int, error) { return map[string]int{"": 0}, nil }
	g.tick()
	if g.st.task(1).FixPart != app.ID || len(g.host.told[app.PaneID]) != 0 {
		t.Fatalf("paused window: fix %d told %v; want held", g.st.task(1).FixPart, g.host.told[app.PaneID])
	}
	g.r.WindowSlots = func() (map[string]int, error) { return map[string]int{"": 1}, nil }
	g.tick()
	if g.st.task(1).Status != Running || len(g.host.told[app.PaneID]) != 1 {
		t.Fatalf("window with room: %s told %d; want sent back", g.st.task(1).Status, len(g.host.told[app.PaneID]))
	}
}

// A waiting send-back that cannot be delivered once a slot frees stops for
// a person: merge failed, the wait spent, the note says why, the part still
// built, and later passes leave it alone.
func TestSendBack_UndeliveredWaitingFixStopsForAPerson(t *testing.T) {
	g := newRig(t)
	app := waitingFix(t, g)
	g.host.deaf = true
	g.doneParts(2)
	g.tick()
	got := g.st.task(1)
	if got.Status != MergeFailed || got.FixPart != 0 || !strings.Contains(got.Note, "send-back to its worker failed") ||
		strings.Contains(got.Note, "waiting for a free slot") || g.st.part(app.ID).Status != Ready {
		t.Fatalf("undelivered = %s fix %d part %s note %q", got.Status, got.FixPart, g.st.part(app.ID).Status, got.Note)
	}
	g.tick()
	if again := g.st.task(1); again.Note != got.Note || again.FixPart != 0 {
		t.Fatalf("a later pass changed it: %+v", again)
	}
}
