package taskq

import (
	"errors"
	"strings"
	"testing"
	"time"
)

// filedIn files task id in window w by the workspace filer.
func (g *rig) filedIn(id int64, w, filer string) {
	for i := range g.st.tasks {
		if g.st.tasks[i].ID == id {
			g.st.tasks[i].WindowID, g.st.tasks[i].FiledBy = w, filer
		}
	}
}

// said is what the filers heard, quiet or waking.
func (g *rig) said(quiet bool) []filerSaid {
	g.host.mu.Lock()
	defer g.host.mu.Unlock()
	var out []filerSaid
	for _, f := range g.host.filed {
		if f.quiet == quiet {
			out = append(out, f)
		}
	}
	return out
}

func TestFiler_ALandedTaskSendsItsFilerOneQuietNotice(t *testing.T) {
	g := builtRig(t, "app", "backend")
	g.filedIn(1, "w1", "ccmux")
	g.tick()
	quiet := g.said(true)
	if len(quiet) != 1 {
		t.Fatalf("quiet notices = %+v, want one", quiet)
	}
	q := quiet[0]
	if q.window != "w1" || q.filer != "ccmux" || q.msgID != "task-landed-1" {
		t.Fatalf("the notice went to %+v, want ccmux in w1 under task-landed-1", q)
	}
	want := `Task #1 "Feature 1" merged: app, backend. 0 task(s) still open in this window's queue.`
	if q.text != want {
		t.Fatalf("notice = %q, want %q", q.text, want)
	}
}

func TestFiler_ALandingActedOnTwiceIsSaidOnce(t *testing.T) {
	g := builtRig(t, "app")
	g.filedIn(1, "w1", "ccmux")
	g.r.Tick()
	job := g.jobs.the(MergeKind)
	res := g.run(job)
	for i := 0; i < 2; i++ { // a daemon cut off after acting on it acts again
		if err := g.apply(job, res); err != nil {
			t.Fatal(err)
		}
	}
	if g.host.filerTry < 3 || len(g.said(true)) != 1 || len(g.said(false)) != 1 {
		t.Fatalf("tried %d times, heard quiet %+v and waking %+v; want each said once", g.host.filerTry, g.said(true), g.said(false))
	}
}

func TestFiler_TheLandingThatEmptiesTheQueueWakesTheFilerOnce(t *testing.T) {
	g := twoBuilt(t, "app")
	g.filedIn(1, "w1", "ccmux")
	g.filedIn(2, "w1", "ccmux")
	g.git.ahead = map[string]int{"/p/app": 2}
	g.tick() // #1 lands, #2 waits its turn
	if q, w := g.said(true), g.said(false); len(q) != 1 || len(w) != 0 {
		t.Fatalf("after the first landing: quiet %+v, waking %+v; want one quiet only", q, w)
	}
	if !strings.Contains(g.said(true)[0].text, "1 task(s) still open") {
		t.Fatalf("the first notice counts #2 as open: %q", g.said(true)[0].text)
	}
	g.now = g.now.Add(time.Minute)
	g.tick() // #2 lands and the queue is empty
	waking := g.said(false)
	if len(g.said(true)) != 2 || len(waking) != 1 {
		t.Fatalf("after the second: quiet %+v, waking %+v; want two and one", g.said(true), waking)
	}
	w := waking[0]
	for _, want := range []string{`#1 "Feature 1"`, `#2 "Feature 2"`, "app: dev is 2 commit(s) ahead of origin/dev",
		"admin: unknown", "Nothing has been pushed.", "/ship", "/push"} {
		if !strings.Contains(w.text, want) {
			t.Fatalf("the drain message lacks %q:\n%s", want, w.text)
		}
	}
	if w.msgID != "queue-drained-w1-2-ccmux" || w.filer != "ccmux" {
		t.Fatalf("drain message %+v, want queue-drained-w1-2-ccmux to ccmux", w)
	}
	g.tick()
	if len(g.said(false)) != 1 {
		t.Fatalf("a later pass said the drain again: %+v", g.said(false))
	}
}

func TestFiler_EachFilerOfTheBatchIsWoken(t *testing.T) {
	g := twoBuilt(t, "app")
	g.filedIn(1, "w1", "ccmux")
	g.filedIn(2, "w1", "admin")
	g.tick()
	g.tick()
	var to []string
	for _, w := range g.said(false) {
		to = append(to, w.filer+"="+w.msgID)
	}
	if strings.Join(to, ",") != "admin=queue-drained-w1-2-admin,ccmux=queue-drained-w1-2-ccmux" {
		t.Fatalf("woken: %v", to)
	}
}

func TestFiler_AnotherWindowsWaitingTaskDoesNotHoldThisDrain(t *testing.T) {
	g := builtRig(t, "app")
	g.filedIn(1, "w1", "ccmux")
	g.feature(2, 0, "admin")
	g.filedIn(2, "w2", "admin")
	g.r.WindowSlots = func() (map[string]int, error) { return map[string]int{"w2": 0}, nil }
	g.tick()
	if got := g.st.task(2); got.Status != Waiting {
		t.Fatalf("#2 should still wait in w2: %+v", got)
	}
	if w := g.said(false); len(w) != 1 || !strings.Contains(w[0].text, "#1") {
		t.Fatalf("w1's drain = %+v, want one naming #1", w)
	}
}

func TestFiler_AFailingTellLeavesTheTaskDone(t *testing.T) {
	g := builtRig(t, "app")
	g.filedIn(1, "w1", "ccmux")
	g.host.filerErr = errors.New("no session answers to ccmux")
	g.tick() // runJobs fails the test if settling the merge errs
	if got := g.st.task(1); got.Status != Done {
		t.Fatalf("task = %+v, want done", got)
	}
}

func TestFiler_ABuilderOrNobodyIsNotTold(t *testing.T) {
	g := twoBuilt(t, "app")
	g.filedIn(1, "w1", "app-task-9")
	g.filedIn(2, "w1", "")
	g.tick()
	g.tick()
	if g.host.filerTry != 0 {
		t.Fatalf("a builder or an empty filer was told: %+v", g.host.filed)
	}
}

func TestFiler_ADrainByCancelSaysWhatLandedAndOnlyThat(t *testing.T) {
	g := twoBuilt(t, "app")
	g.filedIn(1, "w1", "ccmux")
	g.filedIn(2, "w1", "ccmux")
	g.r.Tick() // #1 merging
	g.runJobs()
	cancel := func(id int64) {
		task := g.st.task(id)
		task.Status = Cancelled
		g.st.SaveTask(task)
	}
	cancel(2)
	g.r.Tick()
	if w := g.said(false); len(w) != 1 || !strings.Contains(w[0].text, "#1") || strings.Contains(w[0].text, "#2") {
		t.Fatalf("the drain by cancel = %+v, want one naming #1 only", w)
	}
	g.feature(3, 0, "app")
	g.filedIn(3, "w1", "ccmux")
	cancel(3)
	g.r.Tick()
	if w := g.said(false); len(w) != 1 {
		t.Fatalf("a drain with nothing landed since said something: %+v", w)
	}
}

func TestFiler_NoMarkYetAndNothingLandedSaysNothing(t *testing.T) {
	g := newRig(t)
	g.feature(1, 0, "app")
	g.filedIn(1, "w1", "ccmux")
	task := g.st.task(1)
	task.Status, task.FinishedAt = Done, g.now.UnixMilli() // done before this daemon told anyone
	g.st.SaveTask(task)
	g.r.Tick()
	if g.host.filerTry != 0 {
		t.Fatalf("old history was told: %+v", g.host.filed)
	}
}
