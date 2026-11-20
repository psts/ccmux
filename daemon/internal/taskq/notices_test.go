package taskq

import (
	"errors"
	"log"
	"os"
	"strings"
	"testing"
	"time"

	"ccmux.dev/ccmuxd/internal/model"
	"ccmux.dev/ccmuxd/internal/store"
)

// sentBackUnheard leaves #1's part sent back to a worker nobody on the bus
// hears yet: what a daemon finds that settles a failed merge in its first
// seconds, before any worker's Claude is back on the bus. The worker's pane
// shows the turn that ended long before.
func sentBackUnheard(t *testing.T, g *rig) store.Part {
	t.Helper()
	g.feature(1, 0, "app")
	g.tick()
	g.doneParts(1)
	app := g.partsOf(1)[0]
	g.host.panes[app.PaneID] = PaneState{Attention: model.AttentionNeedsInput, Reason: model.ReasonFinished, At: g.now.Add(-time.Hour).UnixMilli()}
	g.host.deaf = true
	g.host.prompted = func() int64 { return g.now.UnixMilli() }
	g.checks.results["/p/app"] = CheckResult{Configured: true, Output: "FAIL: TestRoute"}
	g.tick()
	return g.st.part(app.ID)
}

// The bus has nobody on it when the failure is settled (the daemon just
// started). The part is saved running again with its notice waiting, the
// watcher does not take the worker's old silence for a new one, and when
// the worker is back on the bus it hears why, once.
func TestNotice_WaitsForAWorkerThatIsNotOnTheBusYet(t *testing.T) {
	g := newRig(t)
	app := sentBackUnheard(t, g)
	task := g.st.task(1)
	if task.Status != Running || task.AutoFixes != 1 || app.Status != Running || app.Note != noteSentBack {
		t.Fatalf("an unheard send-back is still a send-back: task %+v part %+v", task, app)
	}
	if len(g.st.notices) != 1 || !g.st.notices[0].Waiting() || g.st.notices[0].PaneID != app.PaneID || !strings.Contains(g.st.notices[0].Text, "TestRoute") {
		t.Fatalf("its notice waits in the outbox: %+v", g.st.notices)
	}
	for i := 0; i < 3; i++ { // passes while nobody hears
		g.now = g.now.Add(time.Minute)
		g.tick()
	}
	if got := g.st.part(app.ID); got.Status != Running || got.Note != noteSentBack || len(g.host.told[app.PaneID]) != 0 {
		t.Fatalf("while it waits the part is left alone, not nudged and not flagged: %+v told %v", got, g.host.told)
	}
	g.host.deaf = false
	g.tick()
	g.tick()
	told := g.host.told[app.PaneID]
	if len(told) != 1 || !strings.Contains(told[0], "TestRoute") || !strings.Contains(told[0], "back with you") {
		t.Fatalf("the worker hears why once it is on the bus, once: %q", told)
	}
	if n := g.st.notices[0]; n.Waiting() || n.SentAt == 0 || g.st.task(1).AutoFixes != 1 || g.st.task(1).Status != Running {
		t.Fatalf("the notice is written down as sent and the task is as it was: %+v %+v", n, g.st.task(1))
	}
}

// The daemon stops after the bus took the notice and before it wrote that
// down. The next pass says it again under the same id: the bus takes that
// as said, the worker heard it once, and now it is written down.
func TestNotice_SaidAgainAfterACutIsHeardOnce(t *testing.T) {
	g := newRig(t)
	g.feature(1, 0, "app")
	g.tick()
	g.doneParts(1)
	app := g.partsOf(1)[0]
	g.checks.results["/p/app"] = CheckResult{Configured: true, Output: "FAIL: TestRoute"}
	g.st.endErr = errors.New("database is locked")
	g.tick()
	if len(g.host.told[app.PaneID]) != 1 || !g.st.notices[0].Waiting() {
		t.Fatalf("the rig: told, and the notice still reads as waiting: %v %+v", g.host.told, g.st.notices)
	}
	g.st.endErr = nil
	g.tick()
	if len(g.host.told[app.PaneID]) != 1 || g.host.tells != 2 {
		t.Fatalf("said twice, heard once: heard %d, said %d", len(g.host.told[app.PaneID]), g.host.tells)
	}
	if g.st.notices[0].Waiting() {
		t.Fatalf("and now it is written down as sent: %+v", g.st.notices[0])
	}
	g.tick()
	if g.host.tells != 2 {
		t.Fatalf("a sent notice is not said again: %d", g.host.tells)
	}
}

// A worker that never hears it (its Claude runs without the bus) is not
// waited on for good: after NoticeLimit the part asks for a person and says
// why, and the notice is no longer tried.
func TestNotice_AWorkerThatNeverHearsAsksForAPerson(t *testing.T) {
	g := newRig(t)
	app := sentBackUnheard(t, g)
	g.now = g.now.Add(NoticeLimit + time.Minute)
	g.tick()
	got := g.st.part(app.ID)
	if got.Status != NeedsYou || got.Note != noteUntold || g.st.task(1).Status != NeedsYou {
		t.Fatalf("given up on: part %+v task %s", got, g.st.task(1).Status)
	}
	n := g.st.notices[0]
	if n.Waiting() || n.SentAt != 0 || !strings.Contains(n.Failed, "no Claude is on the bus") {
		t.Fatalf("the notice says why it never arrived: %+v", n)
	}
	g.host.deaf = false
	g.tick()
	if len(g.host.told[app.PaneID]) != 0 {
		t.Fatalf("a notice that was given up on is not delivered late: %v", g.host.told)
	}
}

// A part that was cancelled (or started afresh) since has nothing to hear:
// its notice is dropped, never delivered to whoever sits in that pane next.
func TestNotice_ForAPartNoLongerAtWorkIsDropped(t *testing.T) {
	g := newRig(t)
	app := sentBackUnheard(t, g)
	app.Status = Pending
	g.st.SavePart(app)
	g.r.WindowSlots = func() (map[string]int, error) { return map[string]int{"": 0}, nil } // nothing starts
	g.host.deaf = false
	g.tick()
	if n := g.st.notices[0]; n.Waiting() || n.Failed == "" || len(g.host.told[app.PaneID]) != 0 {
		t.Fatalf("dropped, not delivered: %+v told %v", n, g.host.told)
	}
}

// The daemon was down for longer than NoticeLimit with a notice waiting.
// Its first pass after the start finds no worker on the bus yet, as every
// first pass does: that is not a notice nobody took for ten minutes. The
// part stays as it is, and the worker hears why once it is back. A part
// that was never sent back is not touched by any of it.
func TestNotice_ADaemonDownForLongStartsCountingAfresh(t *testing.T) {
	g := newRig(t)
	app := sentBackUnheard(t, g)
	g.feature(2, 0, "backend")
	g.tick()
	other := g.partsOf(2)[0]
	g.host.panes[other.PaneID] = PaneState{Attention: model.AttentionIdle, At: g.now.UnixMilli()}
	g.r.missed = nil // a new daemon
	g.now = g.now.Add(NoticeLimit + time.Hour)
	g.host.panes[other.PaneID] = PaneState{Attention: model.AttentionIdle, At: g.now.UnixMilli()}
	g.tick()
	if got := g.st.part(app.ID); got.Status != Running || got.Note != noteSentBack || !g.st.notices[0].Waiting() {
		t.Fatalf("the first pass after a long stop gave up on a notice it had tried once: %+v %+v", got, g.st.notices[0])
	}
	g.host.deaf = false
	g.tick()
	if told := g.host.told[app.PaneID]; len(told) != 1 || !strings.Contains(told[0], "TestRoute") {
		t.Fatalf("the worker hears why once it is back on the bus: %q", told)
	}
	if got := g.st.part(other.ID); got.Status != Running || got.Note != "" || len(g.host.told[other.PaneID]) != 0 {
		t.Fatalf("a part with no notice was touched: %+v told %v", got, g.host.told[other.PaneID])
	}
	// From this daemon's own first try on, the limit holds as before.
	g.host.deaf = true
	again := sentBackAgain(t, g, app)
	g.now = g.now.Add(NoticeLimit + time.Minute)
	g.tick()
	if got := g.st.part(app.ID); got.Status != NeedsYou || got.Note != noteUntold {
		t.Fatalf("a notice this daemon tried for the whole limit is given up on: %+v (notice %+v)", got, again)
	}
}

// sentBackAgain fails app's merge once more, by a person's send-back with
// nobody on the bus, and returns the new notice.
func sentBackAgain(t *testing.T, g *rig, app store.Part) store.TaskNotice {
	t.Helper()
	task := g.st.task(app.TaskID)
	task.Status = MergeFailed
	g.st.SaveTask(task)
	p := g.st.part(app.ID)
	p.Status = Ready
	g.st.SavePart(p)
	g.host.panes[app.PaneID] = PaneState{Attention: model.AttentionNeedsInput, Reason: model.ReasonFinished, At: g.now.Add(-time.Hour).UnixMilli()}
	if err := g.r.SendBackPart(app.TaskID, app.ID); err != nil {
		t.Fatal(err)
	}
	return g.st.notices[len(g.st.notices)-1]
}

// A notice found waiting (the daemon started with one in the outbox) and
// not taken is tried again for NoticeLimit. Its first miss is logged, with
// the reason, and the passes after it are not: ten minutes of silence must
// not read as nothing wrong, nor fill the log. A send-back that fails at
// once is logged where it happens (sendback.go), and not a second time here.
func TestNotice_TheFirstMissedDeliveryIsLoggedOnce(t *testing.T) {
	g := newRig(t)
	var logged strings.Builder
	log.SetOutput(&logged)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })
	app := sentBackUnheard(t, g)
	g.tick()
	if n := strings.Count(logged.String(), "did not take a notice"); n != 0 {
		t.Fatalf("a miss the send-back already logged is not logged again: %s", logged.String())
	}
	g.r.missed = nil // a new daemon, with the notice waiting
	for i := 0; i < 3; i++ {
		g.now = g.now.Add(time.Minute)
		g.tick()
	}
	if n := strings.Count(logged.String(), "did not take a notice"); n != 1 {
		t.Fatalf("the first miss is logged, once, got %d: %s", n, logged.String())
	}
	if !strings.Contains(logged.String(), "pane "+app.PaneID) || !strings.Contains(logged.String(), "no Claude is on the bus") {
		t.Fatalf("the line names the worker's pane and the reason: %s", logged.String())
	}
}
