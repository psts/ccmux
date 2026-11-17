package runner

import (
	"errors"
	"os"
	"strings"
	"testing"

	"ccmux.dev/ccmuxd/internal/store"
)

func argsOf(t *testing.T, r *rig, id string) (Spec, string) {
	t.Helper()
	sp, err := ReadSpec(r.paths.SpecFile(id))
	if err != nil {
		t.Fatal(err)
	}
	return sp, strings.Join(sp.Args, " ")
}

func TestAThreadMemberStartsReadOnlyWithItsBrief(t *testing.T) {
	r := newRig(t)
	r.sup.cfg.Brief = func(s store.RunnerSession) string {
		if s.ThreadID == 0 {
			return ""
		}
		return "You help thread " + s.Role
	}
	member, err := r.sup.Create(NewSession{Cwd: "/r/api", ThreadID: 3, Role: store.RoleHelper, Repo: "/r/api", ReadOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	sp, args := argsOf(t, r, member.ID)
	if !strings.Contains(args, "--read-only 1") || strings.Contains(args, "--keep-out") || sp.Dir != "/r/api" {
		t.Fatalf("a member starts in its repo's folder, read only: %q in %s", args, sp.Dir)
	}
	if !strings.Contains(args, "--system-prompt-file "+r.paths.BriefFile(member.ID)) {
		t.Fatalf("it is started with its brief: %q", args)
	}
	if text, err := os.ReadFile(r.paths.BriefFile(member.ID)); err != nil || string(text) != "You help thread helper" {
		t.Fatalf("the brief is on disk before the start: %q %v", text, err)
	}
	plain, _ := r.sup.Create(NewSession{Cwd: "/r/app"})
	_, args = argsOf(t, r, plain.ID)
	if strings.Contains(args, "--read-only") || strings.Contains(args, "--keep-out") || strings.Contains(args, "--system-prompt-file") {
		t.Fatalf("a session in no thread has no fence and no brief: %q", args)
	}
	if _, err := os.Stat(r.paths.BriefFile(plain.ID)); !os.IsNotExist(err) {
		t.Fatalf("and no brief file: %v", err)
	}
}

func TestAMemberIsMovedIntoItsWorktreeByAPlannedRestart(t *testing.T) {
	r := newRig(t)
	var heard []store.RunnerSession
	r.sup.Changed = func(s store.RunnerSession) { heard = append(heard, s) }
	member, _ := r.sup.Create(NewSession{Cwd: "/r/api", ThreadID: 3, Role: store.RoleHelper, Repo: "/r/api", ReadOnly: true})
	unit := r.paths.Unit(member.ID)
	wt := "/wt/api/thread-3"
	if err := r.sup.Place(member.ID, store.SessionPlace{Cwd: wt, Worktree: wt, Branch: "thread/3-x"}); err != nil {
		t.Fatal(err)
	}
	if last := heard[len(heard)-1]; last.Cwd != wt || last.ReadOnly {
		t.Fatalf("whoever follows the row hears where it works now: %+v", last)
	}
	if r.units.count("restart") != 0 {
		t.Fatalf("placing it restarts nothing yet: %v", r.units.calls)
	}
	if err := r.sup.Relocate(member.ID); err != nil {
		t.Fatal(err)
	}
	sp, args := argsOf(t, r, member.ID)
	if sp.Dir != wt || !strings.Contains(args, "--keep-out /r/api") || strings.Contains(args, "--read-only") {
		t.Fatalf("it restarts in its worktree, kept out of the repo's folder: %q in %s", args, sp.Dir)
	}
	calls := strings.Join(r.units.calls, ",")
	if !strings.Contains(calls, "reset-failed "+unit+",restart "+unit) {
		t.Fatalf("a planned restart does not count toward the start cap: %v", r.units.calls)
	}
	// Back in the repo's folder after a merge: read only again.
	if err := r.sup.Place(member.ID, store.SessionPlace{Cwd: "/r/api", ReadOnly: true}); err != nil {
		t.Fatal(err)
	}
	r.sup.Relocate(member.ID)
	if sp, args = argsOf(t, r, member.ID); sp.Dir != "/r/api" || !strings.Contains(args, "--read-only 1") || strings.Contains(args, "--keep-out") {
		t.Fatalf("back in its repo's folder it only reads: %q in %s", args, sp.Dir)
	}
}

func TestAnAsleepMemberIsPlacedButNotStarted(t *testing.T) {
	r := newRig(t)
	member, _ := r.sup.Create(NewSession{Cwd: "/r/api", ThreadID: 3, Role: store.RoleHelper, Repo: "/r/api", ReadOnly: true})
	r.sup.SetWanted(member.ID, store.SessionAsleep)
	r.sup.Place(member.ID, store.SessionPlace{Cwd: "/wt/x", Worktree: "/wt/x", Branch: "b"})
	if err := r.sup.Relocate(member.ID); err != nil {
		t.Fatal(err)
	}
	if r.units.count("restart") != 0 {
		t.Fatalf("an asleep session is not started by a move: %v", r.units.calls)
	}
	if got, _ := r.st.RunnerSession(member.ID); got.Cwd != "/wt/x" {
		t.Fatalf("its next start is in the new folder: %+v", got)
	}
	r.sup.SetWanted(member.ID, store.SessionDone)
	if err := r.sup.Place(member.ID, store.SessionPlace{Cwd: "/r/api"}); !errors.Is(err, ErrSessionDone) {
		t.Fatalf("an ended session has no place to change: %v", err)
	}
	if err := r.sup.Place("nope", store.SessionPlace{Cwd: "/r/api"}); !errors.Is(err, store.ErrNoSession) {
		t.Fatalf("an unknown session is an error: %v", err)
	}
}
