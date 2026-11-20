package jobs

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"ccmux.dev/ccmuxd/internal/store"
	"ccmux.dev/ccmuxd/internal/walls"
)

// walledManager is a manager whose jobs start behind walls that open one
// folder, until the test says otherwise.
func walledManager(t *testing.T) (*managerRig, string, *func(New) (*walls.Plan, error)) {
	t.Helper()
	// The rig's folders are under /tmp, which a real unit's walls cannot
	// name; no real unit starts here.
	was := walls.PrivateTemp
	walls.PrivateTemp = nil
	t.Cleanup(func() { walls.PrivateTemp = was })
	r := newManagerRig(t)
	repo := t.TempDir()
	plan := func(New) (*walls.Plan, error) {
		return &walls.Plan{Write: []string{repo}, Outside: []string{"/home/u"}, NoNetwork: true}, nil
	}
	r.m.UnitDir = t.TempDir()
	r.m.Walls = func(n New) (*walls.Plan, error) { return plan(n) }
	return r, repo, &plan
}

func jobDropIn(r *managerRig, id string) string {
	b, err := os.ReadFile(filepath.Join(walls.DropInDir(r.m.UnitDir, r.m.Paths.Unit(id)), "50-ccmux-walls.conf"))
	if err != nil {
		return ""
	}
	return string(b)
}

func TestTheTemplateCapsEveryJob(t *testing.T) {
	text := templateText("/bin/ccmuxd", Paths{State: "/st"}, walls.Settings{MemoryPercent: 50, CPUCores: 2, TasksMax: 1024, JobHours: 2}.Caps())
	for _, want := range []string{"RuntimeMaxSec=7200\n", "MemoryMax=50%\n", "CPUQuota=200%\n", "TasksMax=1024\n", "NoNewPrivileges=yes\n"} {
		if !strings.Contains(text, want) {
			t.Errorf("template misses %q:\n%s", want, text)
		}
	}
	if strings.Contains(text, "ProtectSystem") {
		t.Errorf("walls are one job's drop-in, never the template every job shares:\n%s", text)
	}
}

func TestAWalledJobStartsBehindItsDropInAndIsToldWhatToCheck(t *testing.T) {
	r, repo, _ := walledManager(t)
	job := r.start()
	text := jobDropIn(r, job.ID)
	for _, want := range []string{"ProtectSystem=strict", "ProtectHome=read-only", "PrivateNetwork=yes", repo, r.m.Paths.Dir(job.ID), r.m.Paths.Locks} {
		if !strings.Contains(text, want) {
			t.Errorf("the drop-in misses %q:\n%s", want, text)
		}
	}
	// Of the folder every job's spec is in, it sees its own folder only:
	// another job's spec carries the daemon's environment.
	for _, want := range []string{"TemporaryFileSystem=" + r.m.Paths.State + ":ro\n", "BindPaths=" + r.m.Paths.Dir(job.ID) + "\n"} {
		if !strings.Contains(text, want) {
			t.Errorf("the drop-in misses %q:\n%s", want, text)
		}
	}
	for _, l := range strings.Split(text, "\n") {
		if strings.HasPrefix(l, "ReadWritePaths=") && strings.Contains(l, r.m.Paths.State) {
			t.Errorf("the jobs' folder is not opened whole: %s", l)
		}
	}
	if _, err := os.Stat(r.m.Paths.Locks); err != nil {
		t.Errorf("the locks' folder is made before the start: systemd fails a unit whose walls name a missing folder: %v", err)
	}
	sp, err := ReadSpec(r.m.Paths.SpecFile(job.ID))
	if err != nil {
		t.Fatal(err)
	}
	ns, _ := walls.MountNS()
	if sp.Walls == nil || sp.Walls.HostNS != ns || !sp.Walls.NoNetwork || !slices.Contains(sp.Walls.Refused, "/home/u") || !slices.Contains(sp.Walls.Writable, repo) {
		t.Fatalf("the spec says what the job must find: %+v", sp.Walls)
	}
	if got := sp.Walls.Only[r.m.Paths.State]; len(got) != 1 || got[0] != job.ID {
		t.Fatalf("the job checks that the jobs' folder shows only its own: %+v", sp.Walls.Only)
	}
	if !slices.Contains(sp.Walls.Writable, r.m.Paths.Dir(job.ID)) || !slices.Contains(sp.Walls.Writable, r.m.Paths.Locks) {
		t.Fatalf("and that its own folder and the locks take a write: %v", sp.Walls.Writable)
	}
	if r.units.starts() != 1 {
		t.Fatalf("started once: %v", r.units.asked)
	}
}

// What Claude Code shares between sessions is the job's own behind its
// walls, kept in the job's folder and made before the start.
func TestAWalledJobsPrivateFoldersAreInItsOwnFolder(t *testing.T) {
	r, repo, plan := walledManager(t)
	shared := filepath.Join(t.TempDir(), ".claude", "sessions")
	if err := os.MkdirAll(shared, 0o755); err != nil {
		t.Fatal(err)
	}
	*plan = func(New) (*walls.Plan, error) {
		return &walls.Plan{Write: []string{repo}, Outside: []string{"/home/u"}, Private: []string{shared}}, nil
	}
	job := r.start()
	text := jobDropIn(r, job.ID)
	if !strings.Contains(text, filepath.Join(r.m.Paths.Dir(job.ID), "private")) || !strings.Contains(text, ":"+shared) {
		t.Fatalf("its own folder stands in for the shared one:\n%s", text)
	}
	made, _ := filepath.Glob(filepath.Join(r.m.Paths.Dir(job.ID), "private", "*"))
	if len(made) != 1 {
		t.Fatalf("and is made before the start: %v", made)
	}
}

func TestAJobThatRunsOpenHasNoDropInAndNoCheck(t *testing.T) {
	r, _, plan := walledManager(t)
	*plan = func(New) (*walls.Plan, error) { return nil, nil }
	job := r.start()
	sp, _ := ReadSpec(r.m.Paths.SpecFile(job.ID))
	if jobDropIn(r, job.ID) != "" || sp.Walls != nil {
		t.Fatalf("an open job is what a job was before walls: %q %+v", jobDropIn(r, job.ID), sp.Walls)
	}
}

func TestAHostThatCannotWallStartsNoJob(t *testing.T) {
	r, _, plan := walledManager(t)
	*plan = func(New) (*walls.Plan, error) {
		return nil, errors.New("the sandbox is on, and this host does not wall a unit")
	}
	_, err := r.m.Start(New{Kind: "merge", Subject: "thread:3", Name: "merge-thread-3"})
	if err == nil || !strings.Contains(err.Error(), "does not wall a unit") {
		t.Fatalf("the start says why: %v", err)
	}
	if r.units.starts() != 0 {
		t.Fatalf("a job that was to be walled never starts open: %v", r.units.asked)
	}
	if open, _ := r.st.OpenJobs(); len(open) != 0 {
		t.Fatalf("and leaves no row behind: %+v", open)
	}
}

func TestAnEndedJobLeavesNoWallsFile(t *testing.T) {
	r, _, _ := walledManager(t)
	job := r.start()
	if jobDropIn(r, job.ID) == "" {
		t.Fatal("walled")
	}
	r.ends(job, UnitState{Active: "inactive", Result: "success"}, &Result{ID: job.ID, Output: []byte(`"landed"`)})
	r.m.Tend()
	if got := r.row(job.ID); got.State != store.JobDone {
		t.Fatalf("done: %+v", got)
	}
	if _, err := os.Stat(walls.DropInDir(r.m.UnitDir, r.m.Paths.Unit(job.ID))); !os.IsNotExist(err) {
		t.Fatalf("its drop-in folder is gone with it: %v", err)
	}
}

// A job whose spec says it is walled, in a process that is not: what
// systemd leaves behind on a host that does not allow walls. Nothing of
// the job runs, and its result says why and what to do.
func TestAJobThatIsNotBehindItsWallsRefusesToRun(t *testing.T) {
	ns, err := walls.MountNS()
	if err != nil {
		t.Skipf("no /proc here: %v", err)
	}
	spec := jobDir(t, Spec{ID: "j1", Kind: "count", Walls: &walls.Expect{HostNS: ns, Refused: []string{t.TempDir()}}})
	ran := false
	kinds := map[string]Runner{"count": func(*Run) (any, error) { ran = true; return nil, nil }}
	if err := Exec(spec, kinds); err != nil {
		t.Fatalf("a refusal is a result, not a crash to run again: %v", err)
	}
	res := resultOf(t, spec)
	if ran {
		t.Fatal("a job that is meant to be walled never runs open")
	}
	for _, want := range []string{"the job was not run", "the sandbox is on", "systemd built no walls", "Settings > Sandbox"} {
		if !strings.Contains(res.Error, want) {
			t.Errorf("the result misses %q: %s", want, res.Error)
		}
	}
}
