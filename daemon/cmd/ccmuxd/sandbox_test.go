package main

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"ccmux.dev/ccmuxd/internal/jobs"
	"ccmux.dev/ccmuxd/internal/store"
	"ccmux.dev/ccmuxd/internal/taskq"
	"ccmux.dev/ccmuxd/internal/threads"
	"ccmux.dev/ccmuxd/internal/walls"
)

// testSandbox is a sandbox over settings the test holds and a host that
// walls (or does not), with no real trial unit.
func testSandbox(t *testing.T, set *walls.Settings, hostWalls *bool) *sandbox {
	t.Helper()
	home := t.TempDir()
	run := func(_ context.Context, name string, _ ...string) (string, error) {
		if name == "systemd-run" && !*hostWalls {
			return walls.ErrNoWalls.Error() + ": it runs in the daemon's own mount namespace", errors.New("exit status 97")
		}
		return "", nil
	}
	return &sandbox{
		settings: func() (walls.Settings, error) { return *set, nil },
		home:     home,
		host:     &walls.Prober{Self: "/bin/ccmuxd", Home: home, Run: run},
	}
}

// gitRepo makes a real repo with one worktree under dir.
func gitRepo(t *testing.T, dir string) (repo, worktree string) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("no git")
	}
	repo, worktree = filepath.Join(dir, "app"), filepath.Join(dir, "wt", "app-1", "task-1")
	git := func(in string, args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-c", "user.name=t", "-c", "user.email=t@t"}, args...)...)
		cmd.Dir = in
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	if err := os.MkdirAll(repo, 0o755); err != nil {
		t.Fatal(err)
	}
	git(repo, "init", "-q", "-b", "main")
	git(repo, "commit", "-q", "--allow-empty", "-m", "init")
	git(repo, "worktree", "add", "-q", "-b", "task/1", worktree)
	return repo, worktree
}

func on() walls.Settings {
	s := walls.Defaults()
	s.On = true
	return s
}

func TestOnlyTaskAndThreadWorkIsWalled(t *testing.T) {
	set, host := on(), true
	sb := testSandbox(t, &set, &host)
	repo, worktree := gitRepo(t, t.TempDir())
	for name, sess := range map[string]store.RunnerSession{
		"a person's own session": {Cwd: repo},
		"a repo's own session":   {Cwd: repo, Kind: store.SessionLead},
		"a role agent":           {Cwd: repo, Kind: store.SessionAgent},
	} {
		if plan, err := sb.sessionWalls(sess); plan != nil || err != nil {
			t.Errorf("%s works in the person's own folders and runs open: %+v %v", name, plan, err)
		}
	}
	for _, role := range []string{store.RoleBuilder, store.RoleHelper, store.RoleLead} {
		plan, err := sb.sessionWalls(store.RunnerSession{Role: role, Repo: repo, Worktree: worktree, Cwd: worktree})
		if err != nil || plan == nil {
			t.Fatalf("a %s is walled: %v", role, err)
		}
		if !slices.Contains(plan.Write, worktree) || slices.Contains(plan.Write, repo) || slices.Contains(plan.Write, filepath.Join(repo, ".git")) {
			t.Errorf("a %s writes its worktree, not its repo: %v", role, plan.Write)
		}
	}
}

func TestAWalledSessionsPlan(t *testing.T) {
	set, host := on(), true
	set.ExtraWrite = []string{"/opt/cache"}
	sb := testSandbox(t, &set, &host)
	repo, worktree := gitRepo(t, t.TempDir())
	plan, err := sb.sessionWalls(store.RunnerSession{Role: store.RoleBuilder, Repo: repo, Worktree: worktree, Cwd: worktree})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{filepath.Join(sb.home, ".claude"), filepath.Join(sb.home, ".claude.json"), filepath.Join(sb.home, ".cache"), filepath.Join(sb.home, "go", "pkg", "mod"), "/opt/cache"} {
		if !slices.Contains(plan.Caches, want) {
			t.Errorf("not opened: %s (%v)", want, plan.Caches)
		}
	}
	for _, never := range []string{sb.home, filepath.Join(sb.home, "go"), filepath.Join(sb.home, "go", "bin"), filepath.Join(sb.home, ".config")} {
		if slices.Contains(plan.Caches, never) || slices.Contains(plan.Write, never) {
			t.Errorf("%s must stay closed", never)
		}
	}
	if !slices.Contains(plan.Outside, sb.home) {
		t.Errorf("home is what the check tries a write on: %v", plan.Outside)
	}
	if plan.NoNetwork || plan.ShellBox {
		t.Errorf("a session keeps its network, and has no shell sandbox unless asked: %+v", plan)
	}
	if len(plan.Hidden) != 2 || !strings.HasSuffix(plan.Hidden[0], "/bus") {
		t.Errorf("the user's bus is hidden, or `systemd-run --user` is the way out: %v", plan.Hidden)
	}
}

func TestAMemberThatOnlyReadsYetGetsNoWorktree(t *testing.T) {
	set, host := on(), true
	sb := testSandbox(t, &set, &host)
	repo, _ := gitRepo(t, t.TempDir())
	plan, err := sb.sessionWalls(store.RunnerSession{Role: store.RoleLead, Repo: repo, Cwd: repo, ReadOnly: true})
	if err != nil || plan == nil {
		t.Fatalf("walled: %v", err)
	}
	if len(plan.Write) != 0 {
		t.Fatalf("its repo stays read-only, as its fence has it: %v", plan.Write)
	}
}

func TestTheSettingDecidesAndIsNeverGuessed(t *testing.T) {
	set, host := walls.Defaults(), true
	sb := testSandbox(t, &set, &host)
	repo, worktree := gitRepo(t, t.TempDir())
	builder := store.RunnerSession{Role: store.RoleBuilder, Repo: repo, Worktree: worktree, Cwd: worktree}
	if plan, err := sb.sessionWalls(builder); plan != nil || err != nil {
		t.Fatalf("off: no walls: %+v %v", plan, err)
	}
	sb.settings = func() (walls.Settings, error) { return walls.Settings{}, errors.New("database is locked") }
	if _, err := sb.sessionWalls(builder); err == nil || !strings.Contains(err.Error(), "database is locked") {
		t.Fatalf("settings that cannot be read start nothing: a guess of \"off\" would run it open: %v", err)
	}
	if _, err := sb.jobWalls(jobs.New{Kind: taskq.MergeKind, Input: taskq.LandInput{}}); err == nil {
		t.Fatal("and no job either")
	}
}

func TestAHostThatCannotWallRefusesTheStartAndSaysWhatToDo(t *testing.T) {
	set, host := on(), false
	sb := testSandbox(t, &set, &host)
	repo, worktree := gitRepo(t, t.TempDir())
	_, err := sb.sessionWalls(store.RunnerSession{Role: store.RoleBuilder, Repo: repo, Worktree: worktree, Cwd: worktree})
	if err == nil || !strings.Contains(err.Error(), "does not wall a unit") || !strings.Contains(err.Error(), "Settings > Sandbox") {
		t.Fatalf("session: %v", err)
	}
	_, err = sb.jobWalls(jobs.New{Kind: taskq.MergeKind, Input: taskq.LandInput{Work: []taskq.Work{{RepoPath: repo, Worktree: worktree}}}})
	if err == nil || !strings.Contains(err.Error(), "does not wall a unit") {
		t.Fatalf("job: %v", err)
	}
}

func TestTheShellSandboxIsItsOwnSwitchAndNeedsItsTools(t *testing.T) {
	set, host := on(), true
	set.ShellBox = true
	sb := testSandbox(t, &set, &host)
	repo, worktree := gitRepo(t, t.TempDir())
	builder := store.RunnerSession{Role: store.RoleBuilder, Repo: repo, Worktree: worktree, Cwd: worktree}
	sb.host.Probe()
	can := sb.host.Last().ClaudeSandbox()
	plan, err := sb.sessionWalls(builder)
	switch {
	case can && (err != nil || plan == nil || !plan.ShellBox):
		t.Fatalf("asked for on a host that can: %+v %v", plan, err)
	case !can && (err == nil || !strings.Contains(err.Error(), "sandbox for shell commands")):
		t.Fatalf("asked for on a host that cannot must refuse the start, never leave it out silently: %v", err)
	}
}

func TestAMergeIsWalledIntoItsReposWithHooksAndConfigSealed(t *testing.T) {
	set, host := on(), true
	sb := testSandbox(t, &set, &host)
	repo, worktree := gitRepo(t, t.TempDir())
	plan, err := sb.jobWalls(jobs.New{Kind: taskq.MergeKind, Input: taskq.LandInput{Work: []taskq.Work{{RepoPath: repo, Worktree: worktree}}}})
	if err != nil || plan == nil {
		t.Fatalf("walled: %v", err)
	}
	dot := filepath.Join(repo, ".git")
	for _, want := range []string{repo, dot, filepath.Dir(worktree)} {
		if !slices.Contains(plan.Write, want) {
			t.Errorf("not opened: %s (%v)", want, plan.Write)
		}
	}
	for _, want := range []string{filepath.Join(dot, "hooks"), filepath.Join(dot, "config")} {
		if !slices.Contains(plan.Sealed, want) {
			t.Errorf("not sealed: %s (%v)", want, plan.Sealed)
		}
	}
	if plan.NoNetwork {
		t.Error("a merge runs the repo's checks, which may fetch: it keeps its network")
	}
	if !slices.Contains(plan.Caches, filepath.Join(sb.home, ".claude")) {
		t.Errorf("a repo's checks may drive Claude Code: %v", plan.Caches)
	}
}

func TestATidyUpHasNoNetworkAndAnUnknownJobIsNotStartedOpen(t *testing.T) {
	set, host := on(), true
	sb := testSandbox(t, &set, &host)
	repo, worktree := gitRepo(t, t.TempDir())
	plan, err := sb.jobWalls(jobs.New{Kind: taskq.TidyKind, Input: taskq.TidyInput{Work: []taskq.Work{{RepoPath: repo, Worktree: worktree}}}})
	if err != nil || plan == nil || !plan.NoNetwork {
		t.Fatalf("a tidy-up fetches nothing: %+v %v", plan, err)
	}
	// Its worktree is gone on a second run: the folder it was in still is.
	gone := filepath.Join(filepath.Dir(worktree), "task-9")
	plan, err = sb.jobWalls(jobs.New{Kind: taskq.TidyKind, Input: taskq.TidyInput{Work: []taskq.Work{{RepoPath: repo, Worktree: gone}}}})
	if err != nil || !slices.Contains(plan.Write, filepath.Dir(gone)) {
		t.Fatalf("a worktree that is gone is no reason to fail: %v %v", plan, err)
	}
	if plan, err := sb.jobWalls(jobs.New{Kind: "report", Input: map[string]int{}}); plan != nil || err == nil || !strings.Contains(err.Error(), "report") {
		t.Fatalf("with the sandbox on, a job the walls do not know is not started open: %+v %v", plan, err)
	}
	set.On = false
	if plan, err := sb.jobWalls(jobs.New{Kind: "report", Input: map[string]int{}}); plan != nil || err != nil {
		t.Fatalf("with the sandbox off every job runs open: %+v %v", plan, err)
	}
}

// Every kind in jobKinds must be here with what it is really started
// from: a new kind fails this test until it is added, and then fails it
// again until workOf knows its input.
func TestEveryJobKindThisBinaryRunsIsOneTheWallsKnow(t *testing.T) {
	set, host := on(), true
	sb := testSandbox(t, &set, &host)
	repo, worktree := gitRepo(t, t.TempDir())
	work := []taskq.Work{{RepoPath: repo, Worktree: worktree}}
	startedFrom := map[string]any{
		threads.MergeKind: taskq.LandInput{Work: work},
		taskq.MergeKind:   taskq.LandInput{Work: work},
		taskq.TidyKind:    taskq.TidyInput{Work: work},
	}
	for kind := range jobKinds {
		in, ok := startedFrom[kind]
		if !ok {
			t.Errorf("a job of kind %s is new: name what it is started from here, and teach workOf (sandbox.go) its input", kind)
			continue
		}
		if plan, err := sb.jobWalls(jobs.New{Kind: kind, Input: in}); plan == nil || err != nil {
			t.Errorf("a job of kind %s cannot start with the sandbox on: %v", kind, err)
		}
	}
}

func TestCapsFallBackToTheDefaultsWhenTheSettingsCannotBeRead(t *testing.T) {
	sb := &sandbox{settings: func() (walls.Settings, error) { return walls.Settings{}, errors.New("locked") }}
	if got, want := sb.caps(), walls.Defaults().Caps(); got != want {
		t.Fatalf("a template has to say something: %+v, want %+v", got, want)
	}
}
