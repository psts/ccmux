package runner

import (
	"errors"
	"os"
	"strings"
	"testing"

	"ccmux.dev/ccmuxd/internal/store"
)

// builder is a task's builder as the queue makes it: in its worktree from
// its first moment, with its part's effort, in auto mode.
func builder() NewSession {
	return NewSession{Cwd: "/wt/app/task-12", Name: "app-task-12", Model: "opus", ThreadID: 4, Role: store.RoleBuilder, Repo: "/r/app",
		Worktree: "/wt/app/task-12", Branch: "task/12-export", Effort: "medium", PermissionMode: "auto"}
}

func TestABuilderStartsInItsWorktreeWithItsEffortAndMode(t *testing.T) {
	r := newRig(t)
	b, err := r.sup.Create(builder())
	if err != nil {
		t.Fatal(err)
	}
	row, _ := r.st.RunnerSession(b.ID)
	if row.Worktree != "/wt/app/task-12" || row.Branch != "task/12-export" || row.Effort != "medium" || row.PermissionMode != "auto" {
		t.Fatalf("its row says where it works and what it runs with, from the start: %+v", row)
	}
	sp, args := argsOf(t, r, b.ID)
	if sp.Dir != "/wt/app/task-12" {
		t.Fatalf("it starts in its worktree, not in the repo's folder: %s", sp.Dir)
	}
	for _, want := range []string{"--model opus", "--effort medium", "--permission-mode auto", "--keep-out-files /r/app"} {
		if !strings.Contains(args, want) {
			t.Fatalf("its start line lacks %q: %q", want, args)
		}
	}
	// The full fence would refuse "cp <repo>/.env.local .", which its brief
	// tells it to run; and it never starts reading only.
	if strings.Contains(args, "--keep-out /r/app") || strings.Contains(args, "--read-only") {
		t.Fatalf("a builder has the file fence only: %q", args)
	}
}

func TestAPersonsSessionNamesNoEffortAndNoMode(t *testing.T) {
	r := newRig(t)
	plain, _ := r.sup.Create(NewSession{Cwd: "/r/app", Model: "opus"})
	_, args := argsOf(t, r, plain.ID)
	// The sidecar refuses a flag it does not know, and an empty mode would
	// replace the one the person's settings choose.
	if strings.Contains(args, "--effort") || strings.Contains(args, "--permission-mode") || strings.Contains(args, "--keep-out-files") {
		t.Fatalf("a session that names neither starts as before: %q", args)
	}
}

func TestAThreadMemberInItsWorktreeKeepsTheFullFence(t *testing.T) {
	r := newRig(t)
	wt := "/wt/api/thread-3"
	m, _ := r.sup.Create(NewSession{Cwd: wt, ThreadID: 3, Role: store.RoleHelper, Repo: "/r/api", Worktree: wt, Branch: "thread/3-x"})
	_, args := argsOf(t, r, m.ID)
	if !strings.Contains(args, "--keep-out /r/api") || strings.Contains(args, "--keep-out-files") {
		t.Fatalf("a helper in its worktree is kept out by file and by shell: %q", args)
	}
}

func TestABuilderWhoseBriefCannotBeReadIsNotStartedBlank(t *testing.T) {
	r := newRig(t)
	lost := errors.New("task-12.md: no such file")
	r.sup.cfg.Brief = func(store.RunnerSession) (string, error) { return "", lost }
	b, err := r.sup.Create(builder())
	if err != nil {
		t.Fatal(err)
	}
	if r.units.active[r.paths.Unit(b.ID)] {
		t.Fatal("a builder with no brief must not run: it would not know what to build")
	}
	if !strings.Contains(b.Note, "its brief") || !strings.Contains(b.Note, "no such file") {
		t.Fatalf("the session's note says why it did not start: %q", b.Note)
	}
	if _, err := os.Stat(r.paths.SpecFile(b.ID)); !os.IsNotExist(err) {
		t.Fatalf("and no spec was written for it: %v", err)
	}
}

func TestASessionWithANilLoadoutNamesNoLoadoutFile(t *testing.T) {
	r := newRig(t)
	r.sup.cfg.Loadout = func(s store.RunnerSession) ([]byte, string, error) {
		if s.Role == store.RoleBuilder {
			return nil, "", nil // the user's whole Claude setup
		}
		return []byte(`{}`), `{"signature":"x"}`, nil
	}
	b, _ := r.sup.Create(builder())
	if _, args := argsOf(t, r, b.ID); strings.Contains(args, "--loadout") {
		t.Fatalf("no loadout means no --loadout: the file it would name is not there: %q", args)
	}
	plain, _ := r.sup.Create(NewSession{Cwd: "/r/app"})
	if _, args := argsOf(t, r, plain.ID); !strings.Contains(args, "--loadout "+r.paths.LoadoutFile(plain.ID)) {
		t.Fatalf("an empty loadout is still a loadout: %q", args)
	}
}
