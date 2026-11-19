package runner

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

// walledRig is a rig whose sessions start behind walls that open one
// folder, until the test says otherwise.
type walledRig struct {
	*rig
	unitDir  string
	worktree string
	plan     func(store.RunnerSession) (*walls.Plan, error)
}

func newWalledRig(t *testing.T) *walledRig {
	t.Helper()
	// The rig's folders are under /tmp, which a real unit's walls cannot
	// name; no real unit starts here.
	was := walls.PrivateTemp
	walls.PrivateTemp = nil
	t.Cleanup(func() { walls.PrivateTemp = was })
	w := &walledRig{rig: newRig(t), unitDir: t.TempDir(), worktree: t.TempDir()}
	w.plan = func(store.RunnerSession) (*walls.Plan, error) {
		return &walls.Plan{Write: []string{w.worktree}, Outside: []string{"/home/u"}}, nil
	}
	w.sup.paths.UnitDir, w.paths.UnitDir = w.unitDir, w.unitDir
	w.sup.cfg.Walls = func(sess store.RunnerSession) (*walls.Plan, error) { return w.plan(sess) }
	return w
}

func (w *walledRig) dropIn(id string) string {
	b, err := os.ReadFile(filepath.Join(walls.DropInDir(w.unitDir, w.paths.Unit(id)), "50-ccmux-walls.conf"))
	if err != nil {
		return ""
	}
	return string(b)
}

func (w *walledRig) spec(id string) Spec {
	w.t.Helper()
	sp, err := ReadSpec(w.paths.SpecFile(id))
	if err != nil {
		w.t.Fatal(err)
	}
	return sp
}

func TestTheTemplateCapsEverySessionAndNeverRestartsARefusal(t *testing.T) {
	text := templateText("/bin/ccmuxd", "/st", walls.Settings{MemoryPercent: 60, CPUCores: 2, TasksMax: 4096, JobHours: 3}.Caps())
	for _, want := range []string{"MemoryMax=60%\n", "CPUQuota=200%\n", "TasksMax=4096\n", "NoNewPrivileges=yes\n", "RestartPreventExitStatus=97\n", "Restart=on-failure\n"} {
		if !strings.Contains(text, want) {
			t.Errorf("template misses %q:\n%s", want, text)
		}
	}
	if strings.Contains(text, "RuntimeMaxSec") {
		t.Errorf("a session has no time limit, only a job has:\n%s", text)
	}
	if strings.Contains(text, "ProtectSystem") {
		t.Errorf("walls are one unit's drop-in, never the template every session shares:\n%s", text)
	}
}

func TestAWalledSessionStartsBehindItsDropInAndIsToldWhatToCheck(t *testing.T) {
	w := newWalledRig(t)
	reloads := w.units.reloaded
	sess, err := w.sup.Create(NewSession{Cwd: "/r/app"})
	if err != nil {
		t.Fatal(err)
	}
	text := w.dropIn(sess.ID)
	for _, want := range []string{"ProtectSystem=strict", "ProtectHome=read-only", w.worktree, w.paths.OwnDir(sess.ID), w.paths.MediaDir(sess.ID), w.paths.Runtime} {
		if !strings.Contains(text, want) {
			t.Errorf("the drop-in misses %q:\n%s", want, text)
		}
	}
	if strings.Contains(text, w.paths.State+" ") || strings.Contains(text, w.paths.State+"\n") {
		t.Errorf("the folder every session's spec is in must stay closed:\n%s", text)
	}
	if w.units.reloaded != reloads+1 {
		t.Fatalf("systemd reads the new walls before the start: %d reloads", w.units.reloaded-reloads)
	}
	if n := slices.Index(w.units.calls, "start "+w.paths.Unit(sess.ID)); n < 0 {
		t.Fatalf("started: %v", w.units.calls)
	}
	sp := w.spec(sess.ID)
	ns, _ := walls.MountNS()
	if sp.Walls == nil || sp.Walls.HostNS != ns || !slices.Contains(sp.Walls.Refused, "/home/u") || !slices.Contains(sp.Walls.Writable, w.worktree) {
		t.Fatalf("the spec says what the runner must find: %+v", sp.Walls)
	}
	if got := stateArg(sp); got != w.paths.WalledStateFile(sess.ID) {
		t.Fatalf("a walled session keeps its state in its own folder, not beside every spec: %s", got)
	}
	// The same walls at the next start: nothing to write, nothing to reload.
	w.units.set(w.paths.Unit(sess.ID), false)
	w.reconcile()
	if w.units.reloaded != reloads+1 {
		t.Fatalf("unchanged walls need no reload: %d", w.units.reloaded-reloads)
	}
}

func TestASessionThatRunsOpenHasNoDropInAndNoCheck(t *testing.T) {
	w := newWalledRig(t)
	w.plan = func(store.RunnerSession) (*walls.Plan, error) { return nil, nil }
	sess, _ := w.sup.Create(NewSession{Cwd: "/r/app"})
	if w.dropIn(sess.ID) != "" {
		t.Fatal("an open session has no walls file")
	}
	sp := w.spec(sess.ID)
	if sp.Walls != nil || stateArg(sp) != w.paths.StateFile(sess.ID) {
		t.Fatalf("an open session's spec is what it was before walls: %+v %s", sp.Walls, stateArg(sp))
	}
}

func TestTheStateFileFollowsASessionInAndOutOfItsWalls(t *testing.T) {
	w := newWalledRig(t)
	on := false
	w.plan = func(store.RunnerSession) (*walls.Plan, error) {
		if !on {
			return nil, nil
		}
		return &walls.Plan{Write: []string{w.worktree}, Outside: []string{"/home/u"}}, nil
	}
	sess, _ := w.sup.Create(NewSession{Cwd: "/r/app"})
	open, walled := w.paths.StateFile(sess.ID), w.paths.WalledStateFile(sess.ID)
	if err := os.WriteFile(open, []byte(`{"delivered":["p1"]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	// The Sandbox setting is switched on; the session's next start is walled.
	on = true
	w.units.set(w.paths.Unit(sess.ID), false)
	w.reconcile()
	if b, _ := os.ReadFile(walled); string(b) != `{"delivered":["p1"]}` {
		t.Fatalf("the state file went with it, or the prompts it names would be sent again: %q", b)
	}
	if _, err := os.Stat(open); !os.IsNotExist(err) {
		t.Fatalf("and is not left behind: %v", err)
	}
	if w.dropIn(sess.ID) == "" {
		t.Fatal("walled now")
	}
	// Switched off again.
	on = false
	reloads := w.units.reloaded
	w.units.set(w.paths.Unit(sess.ID), false)
	w.reconcile()
	if b, _ := os.ReadFile(open); string(b) != `{"delivered":["p1"]}` {
		t.Fatalf("and back: %q", b)
	}
	if w.dropIn(sess.ID) != "" || w.units.reloaded != reloads+1 {
		t.Fatalf("an open session's old walls are removed, and systemd told: dropIn=%q reloads=%d", w.dropIn(sess.ID), w.units.reloaded-reloads)
	}
}

// A last spec nobody can read must not block the start that rewrites it:
// the state file stays where it was, and the session's note says so.
func TestAGarbledLastSpecDoesNotBlockTheStart(t *testing.T) {
	w := newWalledRig(t)
	w.plan = func(store.RunnerSession) (*walls.Plan, error) { return nil, nil }
	sess, _ := w.sup.Create(NewSession{Cwd: "/r/app"})
	unit := w.paths.Unit(sess.ID)
	if err := os.WriteFile(w.paths.SpecFile(sess.ID), []byte("{half a spec"), 0o600); err != nil {
		t.Fatal(err)
	}
	w.units.set(unit, false)
	w.reconcile()
	if w.units.count("start "+unit) != 2 {
		t.Fatalf("started again, on a spec written fresh: %v", w.units.calls)
	}
	if sp := w.spec(sess.ID); sp.Node == "" {
		t.Fatalf("the spec is whole again: %+v", sp)
	}
}

func TestAHostThatCannotWallStartsNothing(t *testing.T) {
	w := newWalledRig(t)
	w.plan = func(store.RunnerSession) (*walls.Plan, error) {
		return nil, errors.New("the sandbox is on, and this host does not wall a unit: systemd built no walls")
	}
	sess, _ := w.sup.Create(NewSession{Cwd: "/r/app"})
	if w.units.count("start "+w.paths.Unit(sess.ID)) != 0 {
		t.Fatalf("a session that was to be walled never starts open: %v", w.units.calls)
	}
	if note := w.note(sess.ID); !strings.Contains(note, "this host does not wall a unit") {
		t.Fatalf("its note says why: %q", note)
	}
}

func TestAPlanThatNamesAMissingFolderFailsTheStartWithItsName(t *testing.T) {
	w := newWalledRig(t)
	gone := filepath.Join(w.worktree, "gone")
	w.plan = func(store.RunnerSession) (*walls.Plan, error) { return &walls.Plan{Write: []string{gone}}, nil }
	sess, _ := w.sup.Create(NewSession{Cwd: "/r/app"})
	if w.units.count("start "+w.paths.Unit(sess.ID)) != 0 || !strings.Contains(w.note(sess.ID), gone) {
		t.Fatalf("systemd would fail it with a status that names no path: %v %q", w.units.calls, w.note(sess.ID))
	}
}

func TestASessionWhoseRunnerRefusedIsPutToSleepAndSaysWhatToDo(t *testing.T) {
	w := newWalledRig(t)
	sess, _ := w.sup.Create(NewSession{Cwd: "/r/app"})
	unit := w.paths.Unit(sess.ID)
	// Its runner found no walls and left with the walls' own exit status.
	w.units.set(unit, false)
	w.units.result[unit] = wallsRefused
	w.reconcile()
	got, _ := w.st.RunnerSession(sess.ID)
	if got.Wanted != store.SessionAsleep {
		t.Fatalf("not started again and again on a host that will refuse it again: %+v", got)
	}
	for _, want := range []string{"its walls were not up", "journalctl --user -u " + unit, "Settings > Sandbox"} {
		if !strings.Contains(got.Note, want) {
			t.Errorf("the note misses %q: %s", want, got.Note)
		}
	}
	if w.units.count("start "+unit) != 1 {
		t.Fatalf("no second start: %v", w.units.calls)
	}
	// A person fixed the host and wakes it.
	if _, err := w.sup.Send(sess.ID, "try again", "you"); err != nil {
		t.Fatal(err)
	}
	w.reconcile()
	if w.units.count("reset-failed "+unit) != 1 || w.units.count("start "+unit) != 2 {
		t.Fatalf("a wake clears the refusal and starts it: %v", w.units.calls)
	}
}

func TestAnEndedSessionLeavesNoWallsFile(t *testing.T) {
	w := newWalledRig(t)
	sess, _ := w.sup.Create(NewSession{Cwd: "/r/app"})
	if w.dropIn(sess.ID) == "" {
		t.Fatal("walled")
	}
	if err := w.sup.SetWanted(sess.ID, store.SessionDone); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(walls.DropInDir(w.unitDir, w.paths.Unit(sess.ID))); !os.IsNotExist(err) {
		t.Fatalf("its drop-in folder is gone with it: %v", err)
	}
}

func TestAPlanThatAsksForItRunsShellCommandsInClaudesOwnSandbox(t *testing.T) {
	w := newWalledRig(t)
	cache := t.TempDir()
	box := true
	w.plan = func(store.RunnerSession) (*walls.Plan, error) {
		return &walls.Plan{Write: []string{w.worktree}, Caches: []string{cache, filepath.Join(cache, "not-there")}, Outside: []string{"/home/u"}, ShellBox: box}, nil
	}
	sess, _ := w.sup.Create(NewSession{Cwd: "/r/app"})
	var opened []string
	args := w.spec(sess.ID).Args
	for i := 0; i+1 < len(args); i++ {
		if args[i] == "--shell-box-write" {
			opened = append(opened, args[i+1])
		}
	}
	for _, want := range []string{w.worktree, cache, w.paths.OwnDir(sess.ID)} {
		if !slices.Contains(opened, want) {
			t.Errorf("the second wall opens what the first does; %s is missing: %v", want, opened)
		}
	}
	if slices.Contains(opened, filepath.Join(cache, "not-there")) {
		t.Errorf("a cache this host lacks is not named: %v", opened)
	}
	box = false
	w.units.set(w.paths.Unit(sess.ID), false)
	w.reconcile()
	if slices.Contains(w.spec(sess.ID).Args, "--shell-box-write") {
		t.Fatal("no shell sandbox unless the plan asks for it")
	}
}

func TestSystemdsAnswerIsReadAsARefusalOnlyOnTheWallsOwnStatus(t *testing.T) {
	for out, want := range map[string]string{
		"Result=exit-code\nExecMainStatus=97\n": wallsRefused,
		"ExecMainStatus=97\nResult=exit-code\n": wallsRefused,
		"Result=exit-code\nExecMainStatus=1\n":  "exit-code",
		// After reset-failed the last run's status is still there; the result is not.
		"Result=success\nExecMainStatus=97\n":         "success",
		"Result=start-limit-hit\nExecMainStatus=97\n": startLimitHit,
		"": "",
	} {
		if got := unitResult(out); got != want {
			t.Errorf("%q: got %q, want %q", out, got, want)
		}
	}
}

func TestARunnerThatIsNotBehindItsWallsRefusesToBecomeOne(t *testing.T) {
	ns, err := walls.MountNS()
	if err != nil {
		t.Skipf("no /proc here: %v", err)
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "s1.json")
	// This test's process is in the namespace the spec calls the daemon's:
	// exactly a unit systemd started without walls.
	sp := Spec{Node: "/bin/false", Script: "/x.mjs", Dir: dir, Walls: &walls.Expect{HostNS: ns, Refused: []string{dir}}}
	if err := WriteSpec(path, sp); err != nil {
		t.Fatal(err)
	}
	if err := Exec(path); !errors.Is(err, walls.ErrNoWalls) {
		t.Fatalf("the verb must refuse before it becomes the runner: %v", err)
	}
}
