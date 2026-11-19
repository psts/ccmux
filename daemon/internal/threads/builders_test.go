package threads

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"ccmux.dev/ccmuxd/internal/store"
	"ccmux.dev/ccmuxd/internal/taskq"
)

// builderFor is what the task queue asks for when it starts the part of
// task 12 in the repo called name.
func (r *rig) builderFor(name string) taskq.Builder {
	wt := filepath.Join(r.root, "wt", name+"-abc", "task-12")
	return taskq.Builder{
		TaskID: 12, Title: "Trade CSV export", WindowID: "w1", WorkspaceID: "ws-" + name, Repo: r.repo(name),
		Worktree: wt, Branch: "task/12-trade-csv-export", Name: name + "-task-12",
		Run: taskq.Run{Model: "opus", Effort: "medium"}, Prompt: "Carry out ccmux task #12.",
	}
}

// build starts a builder and returns its session row.
func (r *rig) build(b taskq.Builder) store.RunnerSession {
	r.t.Helper()
	pane, err := r.svc.StartBuilder(b)
	if err != nil {
		r.t.Fatal(err)
	}
	rows, err := r.st.BuildersIn(b.Worktree)
	if err != nil || len(rows) != 1 || rows[0].PaneID != pane {
		r.t.Fatalf("the builder in %s: %+v %v (pane %s)", b.Worktree, rows, err, pane)
	}
	return rows[0]
}

func TestATaskIsAThreadTheQueueLeads(t *testing.T) {
	r := newRig(t)
	app := r.build(r.builderFor("app"))
	th, err := r.st.TaskThread(12)
	if err != nil || th == nil || th.LeadSession != "" || th.State != store.ThreadOpen || th.Group != "w1" || th.Title != "Trade CSV export" {
		t.Fatalf("the task's thread, with no lead: %+v %v", th, err)
	}
	want := r.builderFor("app")
	if app.ThreadID != th.ID || app.Role != store.RoleBuilder || app.Name != "app-task-12" || app.Cwd != want.Worktree || app.Worktree != want.Worktree ||
		app.Branch != want.Branch || app.Repo != r.repo("app") || app.ReadOnly || app.Model != "opus" || app.Effort != "medium" || app.PermissionMode != "auto" ||
		app.WorkspaceID != "ws-app" || app.Title != "Task #12: Trade CSV export" {
		t.Fatalf("a builder starts in its worktree, on its part's run, in auto mode, under its task name: %+v", app)
	}
	if got := r.sess.prompts(app.ID); len(got) != 1 || got[0] != "Carry out ccmux task #12." {
		t.Fatalf("its first prompt: %v", got)
	}
	// The second part joins the same thread; neither leads.
	be := r.build(r.builderFor("backend"))
	if be.ThreadID != th.ID || be.Name != "backend-task-12" {
		t.Fatalf("the second part is in the task's thread: %+v", be)
	}
	d, err := r.svc.Detail(th.ID)
	if err != nil || len(d.Members) != 2 || d.Thread.TaskID != 12 {
		t.Fatalf("the thread's page shows both builders: %+v %v", d, err)
	}
	// Nothing that walks the led threads meets it.
	if list, _ := r.svc.List(); len(list) != 0 {
		t.Fatalf("a task's thread is not among the led ones: %+v", list)
	}
}

// Naming: a builder's teammates go by <repo>-task-<n>, and a repo's plain
// name stays the repo's own session. A led thread rewrites that name to
// its own helper; a task's thread never does.
func TestABuildersMessageToARepoIsNotRerouted(t *testing.T) {
	r := newRig(t)
	app := r.build(r.builderFor("app"))
	r.build(r.builderFor("backend"))
	r.svc.reindex()
	if route, ok := r.svc.Route(app.PaneID, "backend"); ok {
		t.Fatalf("a builder's \"backend\" was rerouted to %+v", route)
	}
	if err := r.svc.StartHelper(app.ThreadID, r.repo("admin")); err == nil || !strings.Contains(err.Error(), "queued task #12") {
		t.Fatalf("a task's thread stands up no helper: %v", err)
	}
	if members, _ := r.st.ThreadMembers(app.ThreadID); len(members) != 2 {
		t.Fatalf("no helper joined: %+v", members)
	}
}

// The queue leads: one part ending takes nobody with it, and what a lead
// may do to its thread is refused with the reason.
func TestOneBuilderEndingStandsNobodyDown(t *testing.T) {
	r := newRig(t)
	app := r.build(r.builderFor("app"))
	be := r.build(r.builderFor("backend"))
	end := func(s store.RunnerSession) {
		t.Helper()
		if err := r.sess.SetWanted(s.ID, store.SessionDone); err != nil {
			t.Fatal(err)
		}
		s.Wanted = store.SessionDone
		r.svc.SessionEnded(s)
	}
	end(app)
	if row, _ := r.st.RunnerSession(be.ID); row.Wanted != store.SessionRunning {
		t.Fatalf("the other part was stood down with it: %+v", row)
	}
	if th, _ := r.st.Thread(app.ThreadID); th.State != store.ThreadOpen {
		t.Fatalf("the thread closed under a builder still at work: %s", th.State)
	}
	if got := r.removed(); got != "" {
		t.Fatalf("a builder's worktree is the queue's to remove, not the thread's: %s", got)
	}
	for name, do := range map[string]func() (string, error){
		"merge":      func() (string, error) { return r.svc.Merge(app.ThreadID, ByPerson) },
		"end":        func() (string, error) { return r.svc.End(app.ThreadID, ByPerson) },
		"stand down": func() (string, error) { return r.svc.StandDown(app.ThreadID, "backend", ByPerson) },
		"stop merge": func() (string, error) { return r.svc.StopMerge(app.ThreadID, ByPerson) },
	} {
		if _, err := do(); err == nil || !strings.Contains(err.Error(), "queued task #12") {
			t.Errorf("%s of a task's thread: %v", name, err)
		}
	}
	if row, _ := r.st.RunnerSession(be.ID); row.Wanted != store.SessionRunning {
		t.Fatalf("a refused action still ended a builder: %+v", row)
	}
	// The last one out closes the thread; a part built again opens it.
	end(be)
	if th, _ := r.st.Thread(app.ThreadID); th.State != store.ThreadEnded {
		t.Fatalf("the thread stays open with no builder left: %s", th.State)
	}
	again := r.build(r.builderFor("app"))
	if th, _ := r.st.Thread(app.ThreadID); again.ThreadID != app.ThreadID || th.State != store.ThreadOpen {
		t.Fatalf("a part built again joins the task's own thread, open again: %+v %s", again, th.State)
	}
}

func TestABuilderThatCannotBeToldToStartIsEnded(t *testing.T) {
	r := newRig(t)
	r.sess.sendErr = errors.New("registry locked")
	b := r.builderFor("app")
	if _, err := r.svc.StartBuilder(b); err == nil || !strings.Contains(err.Error(), "registry locked") {
		t.Fatalf("a first prompt that was not queued fails the start: %v", err)
	}
	if rows, _ := r.st.BuildersIn(b.Worktree); len(rows) != 0 {
		t.Fatalf("a builder with nothing to do was left running: %+v", rows)
	}
	// No ccmux session for the repo: no pane, no bus, so it could never report.
	r.sess.sendErr = nil
	b.WorkspaceID = ""
	if _, err := r.svc.StartBuilder(b); err == nil || !strings.Contains(err.Error(), "no pane") {
		t.Fatalf("a builder with no pane fails the start: %v", err)
	}
	if rows, _ := r.st.BuildersIn(b.Worktree); len(rows) != 0 {
		t.Fatalf("a builder off the bus was left running: %+v", rows)
	}
	r.sess.createErr = errors.New("no systemd")
	if _, err := r.svc.StartBuilder(r.builderFor("app")); err == nil || !strings.Contains(err.Error(), "no systemd") {
		t.Fatalf("a session that cannot be made fails the start: %v", err)
	}
}

// A builder's standing prompt is its part's brief, from the file the queue
// wrote; without it the start fails. It never gets a helper's brief, which
// says "ask the lead" and "raise a question card".
func TestABuildersBriefIsItsPartsBrief(t *testing.T) {
	r := newRig(t)
	b := r.builderFor("app")
	sess := r.build(b)
	if _, err := r.svc.SessionBrief(sess); err == nil {
		t.Fatal("a builder whose brief is gone must not start blank")
	}
	if err := os.MkdirAll(filepath.Dir(b.Worktree), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(taskq.BriefFile(b.Worktree), []byte("# ccmux task #12: Trade CSV export\n\nDo not ask with a question card."), 0o644); err != nil {
		t.Fatal(err)
	}
	text, err := r.svc.SessionBrief(sess)
	if err != nil || !strings.HasPrefix(text, "# ccmux task #12") || strings.Contains(text, "helper") {
		t.Fatalf("its brief: %q %v", text, err)
	}
	// A led thread's member and a plain session are as before.
	_, lead := r.thread("Live work")
	if text, err := r.svc.SessionBrief(lead); err != nil || !strings.Contains(text, "You lead ccmux thread") {
		t.Fatalf("a lead's brief: %q %v", text, err)
	}
	if text, err := r.svc.SessionBrief(store.RunnerSession{ID: "plain"}); err != nil || text != "" {
		t.Fatalf("a session in no thread has none: %q %v", text, err)
	}
}

func TestTheThreadToolAndFirstEditAreNotABuilders(t *testing.T) {
	r := newRig(t)
	app := r.build(r.builderFor("app"))
	if _, err := r.svc.Tool(app.ID, "merge", ""); err == nil || !strings.Contains(err.Error(), "tasks tool") || strings.Contains(err.Error(), "helper") {
		t.Fatalf("a builder's thread tool: %v", err)
	}
	moved, err := r.svc.FirstEdit(app.ID)
	if err != nil || !strings.Contains(moved.Message, app.Worktree) {
		t.Fatalf("a stray first-edit answers with where it works: %+v %v", moved, err)
	}
	if got := r.sess.moves(); len(got) != 0 {
		t.Fatalf("and restarts nothing in the middle of its work: %v", got)
	}
}
