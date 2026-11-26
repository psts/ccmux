package taskq

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func gitRepo(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	repo := t.TempDir()
	for _, args := range [][]string{
		{"init", "-q", "-b", "main"},
		{"-c", "user.email=t@t", "-c", "user.name=t", "commit", "-q", "--allow-empty", "-m", "init"},
	} {
		if out, err := exec.Command("git", append([]string{"-C", repo}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
	}
	return repo
}

func hasBranch(t *testing.T, repo, branch string) bool {
	t.Helper()
	ok, err := branchExists(repo, branch)
	if err != nil {
		t.Fatal(err)
	}
	return ok
}

// The trunk is dev where there is one, main otherwise; a repo with neither
// (and no master) builds from what is checked out.
func TestExecGit_Trunk(t *testing.T) {
	repo := gitRepo(t)
	g := ExecGit{}
	if got, err := g.Trunk(repo); err != nil || got != "main" {
		t.Fatalf("main only = %q, %v", got, err)
	}
	if _, err := runGit(repo, "branch", "dev"); err != nil {
		t.Fatal(err)
	}
	if got, _ := g.Trunk(repo); got != "dev" {
		t.Fatalf("with dev = %q, want dev", got)
	}
	other := gitRepo(t)
	if _, err := runGit(other, "branch", "-m", "main", "trunk"); err != nil {
		t.Fatal(err)
	}
	if got, err := g.Trunk(other); err != nil || got != "trunk" {
		t.Fatalf("no dev, main or master = %q, %v; want the checked-out branch", got, err)
	}
}

// A worktree is made on a new branch; one that still exists is kept as it
// is; one deleted by hand comes back on its existing branch.
func TestExecGit_AddWorktree(t *testing.T) {
	repo := gitRepo(t)
	g := ExecGit{}
	wt := filepath.Join(t.TempDir(), "admin", "task-1")
	if err := g.AddWorktree(repo, wt, "task/1-x", "main"); err != nil {
		t.Fatal(err)
	}
	if got, _ := runGit(wt, "branch", "--show-current"); got != "task/1-x" {
		t.Fatalf("new worktree on %q", got)
	}
	marker := filepath.Join(wt, "work-in-progress")
	if err := os.WriteFile(marker, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := g.AddWorktree(repo, wt, "task/1-x", "main"); err != nil {
		t.Fatalf("existing worktree: %v", err)
	}
	if _, err := os.Stat(marker); err != nil {
		t.Fatal("an existing worktree was replaced, losing its work")
	}
	if err := os.RemoveAll(wt); err != nil {
		t.Fatal(err)
	}
	if err := g.AddWorktree(repo, wt, "task/1-x", "main"); err != nil {
		t.Fatalf("worktree deleted by hand: %v", err)
	}
	if got, _ := runGit(wt, "branch", "--show-current"); got != "task/1-x" {
		t.Fatalf("re-added worktree on %q", got)
	}
}

// The repo's untracked Claude settings follow it into the worktree, and a
// repo without any copies nothing.
func TestCopyLocalSettings(t *testing.T) {
	repo, wt := t.TempDir(), t.TempDir()
	if err := copyLocalSettings(repo, wt); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(wt, ".claude")); !os.IsNotExist(err) {
		t.Fatal("settings made up for a repo with none")
	}
	os.MkdirAll(filepath.Join(repo, ".claude"), 0o755)
	os.WriteFile(filepath.Join(repo, ".claude", "settings.local.json"), []byte(`{"qa":1}`), 0o644)
	if err := copyLocalSettings(repo, wt); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(filepath.Join(wt, ".claude", "settings.local.json")); string(got) != `{"qa":1}` {
		t.Fatalf("copied = %q", got)
	}
}

func commitFile(t *testing.T, dir, name, text string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(text), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"add", name}, {"-c", "user.email=t@t", "-c", "user.name=t", "commit", "-q", "-m", name}} {
		if _, err := runGit(dir, args...); err != nil {
			t.Fatal(err)
		}
	}
}

// A part's branch is rebased onto work that landed on main meanwhile, then
// main moves forward to it in the repo's own clean folder; a dirty folder
// blocks it; and a clash is undone, leaving the branch as it was.
func TestExecGit_MergeSteps(t *testing.T) {
	repo := gitRepo(t)
	g := ExecGit{}
	wt := filepath.Join(t.TempDir(), "task-1")
	if err := g.AddWorktree(repo, wt, "task/1-x", "main"); err != nil {
		t.Fatal(err)
	}
	commitFile(t, wt, "feature.txt", "new")
	commitFile(t, repo, "other.txt", "landed meanwhile")

	os.WriteFile(filepath.Join(repo, "other.txt"), []byte("typing"), 0o644)
	if why, err := g.TrunkBlocked(repo, "main"); err != nil || why.Kind != BlockUncommitted || !strings.Contains(why.Why, "uncommitted") {
		t.Fatalf("dirty folder = %q, %v", why, err)
	}
	runGit(repo, "checkout", "--", "other.txt")
	os.WriteFile(filepath.Join(repo, "untracked.txt"), []byte("x"), 0o644)
	if why, _ := g.TrunkBlocked(repo, "main"); why.Blocked() {
		t.Fatalf("an untracked file blocked the merge: %q", why)
	}

	rev, err := g.Rebase(wt, "main")
	if err != nil || rev == "" {
		t.Fatalf("rebase = %q, %v", rev, err)
	}
	commitFile(t, wt, "later.txt", "made after the checks ran")
	if err := g.FastForward(repo, rev, "main"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(repo, "later.txt")); !os.IsNotExist(err) {
		t.Fatal("a commit made after the checks was merged")
	}
	if _, err := os.Stat(filepath.Join(repo, "feature.txt")); err != nil {
		t.Fatal("the repo's own folder did not get the merged work")
	}
	if err := g.RemoveWorktree(repo, wt, "task/1-x", "main"); err == nil {
		t.Fatal("a branch holding an unmerged commit was deleted")
	}
	if _, err := os.Stat(wt); !os.IsNotExist(err) {
		t.Fatal("worktree left after the merge")
	}
}

func TestExecGit_RebaseClashIsUndone(t *testing.T) {
	repo := gitRepo(t)
	g := ExecGit{}
	wt := filepath.Join(t.TempDir(), "task-1")
	g.AddWorktree(repo, wt, "task/1-x", "main")
	commitFile(t, wt, "same.txt", "mine")
	commitFile(t, repo, "same.txt", "theirs")
	_, err := g.Rebase(wt, "main")
	var clash rebaseClash
	if !errors.As(err, &clash) || !strings.Contains(err.Error(), "clashes") {
		t.Fatalf("clash = %v, want a rebaseClash its worker can fix", err)
	}
	if got, _ := os.ReadFile(filepath.Join(wt, "same.txt")); string(got) != "mine" {
		t.Fatalf("the clash was not undone: %q", got)
	}
	if err := g.FastForward(repo, "task/1-x", "main"); err == nil {
		t.Fatal("a branch that is not ahead of main was fast-forwarded")
	}
}

// With another branch checked out in the repo's folder, the trunk moves
// without touching that folder's files.
func TestExecGit_FastForwardOffTrunk(t *testing.T) {
	repo := gitRepo(t)
	g := ExecGit{}
	wt := filepath.Join(t.TempDir(), "task-1")
	g.AddWorktree(repo, wt, "task/1-x", "main")
	commitFile(t, wt, "feature.txt", "new")
	runGit(repo, "checkout", "-q", "-b", "mine")
	os.WriteFile(filepath.Join(repo, "README"), []byte("typing on my branch"), 0o644)
	if why, _ := g.TrunkBlocked(repo, "main"); why.Blocked() {
		t.Fatalf("off-trunk folder blocked: %q", why)
	}
	rev, _ := runGit(wt, "rev-parse", "HEAD")
	if err := g.FastForward(repo, rev, "main"); err != nil {
		t.Fatal(err)
	}
	if got, _ := runGit(repo, "rev-parse", "main"); got == "" {
		t.Fatal("main unreadable")
	}
	if _, err := os.Stat(filepath.Join(repo, "feature.txt")); !os.IsNotExist(err) {
		t.Fatal("the folder on another branch got the merged files")
	}
	if ahead, _ := runGit(repo, "log", "--oneline", "mine..main"); ahead == "" {
		t.Fatal("main did not move")
	}
}

// Uncommitted work shows, untracked files too; the settings the runner
// copied in do not.
func TestExecGit_Uncommitted(t *testing.T) {
	repo := gitRepo(t)
	g := ExecGit{}
	wt := filepath.Join(t.TempDir(), "task-1")
	g.AddWorktree(repo, wt, "task/1-x", "main")
	os.MkdirAll(filepath.Join(wt, ".claude"), 0o755)
	os.WriteFile(filepath.Join(wt, localSettings), []byte("{}"), 0o644)
	if left, err := g.Uncommitted(wt); err != nil || left != "" {
		t.Fatalf("only the copied settings = %q, %v", left, err)
	}
	os.MkdirAll(filepath.Join(wt, "pkg"), 0o755)
	os.WriteFile(filepath.Join(wt, "pkg", "export.go"), []byte("package pkg"), 0o644)
	if left, _ := g.Uncommitted(wt); !strings.Contains(left, "pkg/export.go") {
		t.Fatalf("an untracked file = %q", left)
	}
}

// A trunk checked out in another worktree is left alone, and an off-trunk
// move refuses a trunk that moved past the checked commit.
func TestExecGit_TrunkElsewhereAndMoved(t *testing.T) {
	repo := gitRepo(t)
	g := ExecGit{}
	wt := filepath.Join(t.TempDir(), "task-1")
	g.AddWorktree(repo, wt, "task/1-x", "main")
	commitFile(t, wt, "feature.txt", "new")
	rev, _ := runGit(wt, "rev-parse", "HEAD")
	runGit(repo, "checkout", "-q", "-b", "mine")
	other := filepath.Join(t.TempDir(), "main-wt")
	if _, err := runGit(repo, "worktree", "add", other, "main"); err != nil {
		t.Fatal(err)
	}
	if why, _ := g.TrunkBlocked(repo, "main"); why.Kind != BlockElsewhere || !strings.Contains(why.Why, "checked out in") {
		t.Fatalf("trunk in another worktree = %q", why)
	}
	runGit(repo, "worktree", "remove", other)
	if why, _ := g.TrunkBlocked(repo, "main"); why.Blocked() {
		t.Fatalf("trunk free again = %q", why)
	}
	runGit(repo, "checkout", "-q", "main")
	commitFile(t, repo, "moved.txt", "main moved")
	runGit(repo, "checkout", "-q", "mine")
	if err := g.FastForward(repo, rev, "main"); err == nil || !strings.Contains(err.Error(), "moved") {
		t.Fatalf("fast-forward past a moved trunk = %v", err)
	}
}

// Cleanup forces nothing: the settings the runner copied in go, but a file
// written after the merge's check keeps the worktree, and a commit the
// trunk lacks keeps the branch.
func TestExecGit_RemoveWorktreeNeverForces(t *testing.T) {
	repo := gitRepo(t)
	g := ExecGit{}
	wt := filepath.Join(t.TempDir(), "task-1")
	g.AddWorktree(repo, wt, "task/1-x", "main")
	os.MkdirAll(filepath.Join(wt, ".claude"), 0o755)
	os.WriteFile(filepath.Join(wt, localSettings), []byte("{}"), 0o644)
	os.WriteFile(filepath.Join(wt, "late.go"), []byte("package late"), 0o644)
	if err := g.RemoveWorktree(repo, wt, "task/1-x", "main"); err == nil || !strings.Contains(err.Error(), "uncommitted") {
		t.Fatalf("a worktree with a late file = %v", err)
	}
	if _, err := os.Stat(filepath.Join(wt, "late.go")); err != nil {
		t.Fatal("the late file was deleted")
	}
	os.Remove(filepath.Join(wt, "late.go"))
	if err := g.RemoveWorktree(repo, wt, "task/1-x", "main"); err != nil {
		t.Fatalf("a worktree with only the copied settings = %v", err)
	}
	if _, err := os.Stat(wt); !os.IsNotExist(err) || hasBranch(t, repo, "task/1-x") {
		t.Fatal("clean worktree or its merged branch left behind")
	}
}

// With the repo's folder on another branch, a merged part's branch is
// still deleted: the trunk holds its commits, whatever the folder's branch.
func TestExecGit_RemoveAfterAnOffTrunkMerge(t *testing.T) {
	repo := gitRepo(t)
	g := ExecGit{}
	wt := filepath.Join(t.TempDir(), "task-1")
	g.AddWorktree(repo, wt, "task/1-x", "main")
	commitFile(t, wt, "feature.txt", "new")
	rev, _ := runGit(wt, "rev-parse", "HEAD")
	runGit(repo, "checkout", "-q", "-b", "mine")
	if err := g.FastForward(repo, rev, "main"); err != nil {
		t.Fatal(err)
	}
	if err := g.RemoveWorktree(repo, wt, "task/1-x", "main"); err != nil {
		t.Fatalf("remove after an off-trunk merge = %v", err)
	}
	if hasBranch(t, repo, "task/1-x") {
		t.Fatal("the merged branch was kept")
	}
}

// A detached folder has no branch checked out; a folder git cannot read is
// an error, never taken for "the trunk is not checked out here".
func TestCheckedOut(t *testing.T) {
	repo := gitRepo(t)
	if cur, err := checkedOut(repo); err != nil || cur != "main" {
		t.Fatalf("on main = %q, %v", cur, err)
	}
	runGit(repo, "checkout", "-q", "--detach")
	if cur, err := checkedOut(repo); err != nil || cur != "" {
		t.Fatalf("detached = %q, %v", cur, err)
	}
	if _, err := checkedOut(t.TempDir()); err == nil {
		t.Fatal("a folder that is no repo gave no error")
	}
	if why, err := (ExecGit{}).TrunkBlocked(t.TempDir(), "main"); err == nil {
		t.Fatalf("TrunkBlocked on an unreadable folder = %q, no error", why)
	}
}

// ChangedUnder sees an edited, an added and a renamed file under the
// directory, by their real names, and nothing outside it.
func TestExecGit_ChangedUnder(t *testing.T) {
	repo := gitRepo(t)
	os.MkdirAll(filepath.Join(repo, ".claude", "gates"), 0o755)
	commitFile(t, repo, ".claude/gates/frözen.txt", "a\n")
	commitFile(t, repo, ".claude/gates/run.sh", "true\n")
	g := ExecGit{}
	wt := filepath.Join(t.TempDir(), "task-1")
	g.AddWorktree(repo, wt, "task/1-x", "main")
	if got, err := g.ChangedUnder(repo, "main", "task/1-x", ".claude"); err != nil || len(got) != 0 {
		t.Fatalf("untouched = %v, %v", got, err)
	}
	commitFile(t, wt, ".claude/gates/frözen.txt", "a\nb\n")
	commitFile(t, wt, "src.go", "package src")
	runGit(wt, "mv", ".claude/gates/run.sh", ".claude/gates/run2.sh")
	runGit(wt, "-c", "user.email=t@t", "-c", "user.name=t", "commit", "-q", "-m", "mv")
	got, err := g.ChangedUnder(repo, "main", "task/1-x", ".claude")
	want := ".claude/gates/frözen.txt,.claude/gates/run.sh,.claude/gates/run2.sh"
	if err != nil || strings.Join(got, ",") != want {
		t.Fatalf("changed = %v, %v; want %s", got, err, want)
	}
}

// Moved sees a commit made after the checks and a tracked file changed,
// but not a new untracked file, which never lands.
func TestExecGit_Moved(t *testing.T) {
	repo := gitRepo(t)
	g := ExecGit{}
	wt := worktreeOf(t, repo)
	commitFile(t, wt, "feature.txt", "new")
	rev, _ := runGit(wt, "rev-parse", "HEAD")
	os.MkdirAll(filepath.Join(wt, ".claude"), 0o755)
	os.WriteFile(filepath.Join(wt, localSettings), []byte("{}"), 0o644)
	os.WriteFile(filepath.Join(wt, "report.xml"), []byte("<ok/>"), 0o644) // a check's output nobody ignored
	if why, err := g.Moved(wt, rev); why != "" || err != nil {
		t.Fatalf("unchanged (copied settings, a check's new file) = %q, %v", why, err)
	}
	os.WriteFile(filepath.Join(wt, "feature.txt"), []byte("reformatted"), 0o644)
	if why, _ := g.Moved(wt, rev); !strings.Contains(why, "tracked files changed") || !strings.Contains(why, "feature.txt") {
		t.Fatalf("edited after the checks = %q", why)
	}
	commitFile(t, wt, "feature.txt", "reformatted")
	if why, _ := g.Moved(wt, rev); !strings.Contains(why, "commit the checks did not run on") {
		t.Fatalf("committed after the checks = %q", why)
	}
}

// A git failure is said as one: a trunk that cannot be read is neither "it
// moved" nor "the branch has commits it lacks", and no branch counts as
// missing when git could not look.
func TestExecGit_GitErrorsAreNotNo(t *testing.T) {
	repo := gitRepo(t)
	g := ExecGit{}
	if ok, err := gitYes(repo, "merge-base", "--is-ancestor", "HEAD", "no-such-ref"); ok || err == nil {
		t.Fatalf("bad ref = %v, %v; want an error", ok, err)
	}
	if ok, err := gitYes(repo, "rev-parse", "--verify", "--quiet", "refs/heads/nope"); ok || err != nil {
		t.Fatalf("missing branch = %v, %v; want a plain no", ok, err)
	}
	if _, err := branchExists(filepath.Join(repo, "not-a-dir"), "main"); err == nil {
		t.Fatal("a folder git cannot open read as a missing branch")
	}
	wt := worktreeOf(t, repo)
	commitFile(t, wt, "feature.txt", "new")
	rev, _ := runGit(wt, "rev-parse", "HEAD")
	runGit(repo, "checkout", "-q", "--detach")
	if err := g.FastForward(repo, rev, "no-such-trunk"); err == nil || strings.Contains(err.Error(), "moved since") {
		t.Fatalf("unreadable trunk on fast-forward = %v", err)
	}
	if err := g.RemoveWorktree(repo, wt, "task/1-x", "no-such-trunk"); err == nil || !strings.Contains(err.Error(), "comparing it with no-such-trunk failed") {
		t.Fatalf("unreadable trunk on cleanup = %v", err)
	}
	if !hasBranch(t, repo, "task/1-x") {
		t.Fatal("the branch went although it could not be compared")
	}
}

// A git index lock in the repo blocks a merge with its path; without one,
// a clean folder on its trunk is not blocked.
func TestExecGit_TrunkBlockedByAGitLock(t *testing.T) {
	repo := gitRepo(t)
	g := ExecGit{}
	if why, err := g.TrunkBlocked(repo, "main"); err != nil || why.Blocked() {
		t.Fatalf("clean folder = %q, %v", why, err)
	}
	lock := filepath.Join(repo, ".git", "index.lock")
	if err := os.WriteFile(lock, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	why, err := g.TrunkBlocked(repo, "main")
	if err != nil || why.Kind != BlockLock || why.Lock != lock || why.LockTime.IsZero() || !strings.Contains(why.Why, lock) || !strings.Contains(why.Why, "git is busy") {
		t.Fatalf("locked folder = %q, %v", why, err)
	}
}

// A git command past its time says so: "signal: killed" alone read as a
// folder that would not go, when the removal had only run out of time.
func TestRunGitFor_SaysItTimedOut(t *testing.T) {
	repo := gitRepo(t)
	_, err := runGitFor(time.Nanosecond, repo, "status")
	if err == nil || !errors.Is(err, errGitTimeout) || !strings.Contains(err.Error(), "took longer than") {
		t.Fatalf("timed-out git = %v", err)
	}
	if _, err := runGitFor(time.Minute, repo, "status"); err != nil {
		t.Fatalf("git inside its time = %v", err)
	}
}

// A tidy-up that is run again (the daemon was cut off between the worktree
// and the branch, or right after both) finishes what is left and is not an
// error: the worktree's folder being gone is where it was headed.
func TestExecGit_RemoveWorktreeCanBeAskedAgain(t *testing.T) {
	repo := gitRepo(t)
	g := ExecGit{}
	wt := filepath.Join(t.TempDir(), "thread-1")
	g.AddWorktree(repo, wt, "thread/1-x", "main")
	// Cut off after the folder went, before the branch did.
	if _, err := runGit(repo, "worktree", "remove", wt); err != nil {
		t.Fatal(err)
	}
	if !hasBranch(t, repo, "thread/1-x") {
		t.Fatal("the rig: the branch is still there")
	}
	if err := g.RemoveWorktree(repo, wt, "thread/1-x", "main"); err != nil {
		t.Fatalf("asked again with the folder gone = %v", err)
	}
	if hasBranch(t, repo, "thread/1-x") {
		t.Fatal("the branch the first try left was not finished")
	}
	if err := g.RemoveWorktree(repo, wt, "thread/1-x", "main"); err != nil {
		t.Fatalf("asked a third time, with nothing left = %v", err)
	}
	// A folder deleted by hand leaves git's record of the worktree; that goes too.
	other := filepath.Join(t.TempDir(), "thread-2")
	g.AddWorktree(repo, other, "thread/2-y", "main")
	os.RemoveAll(other)
	if err := g.RemoveWorktree(repo, other, "thread/2-y", "main"); err != nil {
		t.Fatalf("a folder that vanished = %v", err)
	}
	if list, _ := runGit(repo, "worktree", "list"); strings.Contains(list, "thread-2") || hasBranch(t, repo, "thread/2-y") {
		t.Fatalf("git still lists the vanished worktree, or kept its branch: %s", list)
	}
	// And a branch with work the trunk lacks is still never deleted.
	third := filepath.Join(t.TempDir(), "thread-3")
	g.AddWorktree(repo, third, "thread/3-z", "main")
	commitFile(t, third, "kept.txt", "work")
	runGit(repo, "worktree", "remove", third)
	if err := g.RemoveWorktree(repo, third, "thread/3-z", "main"); err == nil || !strings.Contains(err.Error(), "it has commits main does not") || !hasBranch(t, repo, "thread/3-z") {
		t.Fatalf("unmerged work is kept, asked again or not: %v", err)
	}
}

// DropScratch removes a scratch copy and its branch whatever is in them (an
// uncommitted file, a commit the trunk lacks), is done when asked again,
// clears git's record of a folder already gone, and leaves every other
// worktree and branch alone.
func TestDropScratchForcesItsOwnCopyOnly(t *testing.T) {
	repo := gitRepo(t)
	g := ExecGit{}
	dir := t.TempDir()
	wt, other, gone := filepath.Join(dir, "thread-1"), filepath.Join(dir, "task-2"), filepath.Join(dir, "thread-3")
	for _, w := range []struct{ path, branch string }{{wt, "thread/1-x"}, {other, "task/2-y"}, {gone, "thread/3-z"}} {
		if err := g.AddWorktree(repo, w.path, w.branch, "main"); err != nil {
			t.Fatal(err)
		}
	}
	commitFile(t, wt, "unmerged.txt", "a commit main lacks")
	os.WriteFile(filepath.Join(wt, "count.txt"), []byte("10"), 0o644)
	if err := DropScratch(repo, wt, "thread/1-x"); err != nil {
		t.Fatalf("drop = %v", err)
	}
	if _, err := os.Stat(wt); !os.IsNotExist(err) || hasBranch(t, repo, "thread/1-x") {
		t.Fatal("the scratch copy or its branch stayed")
	}
	if err := DropScratch(repo, wt, "thread/1-x"); err != nil {
		t.Fatalf("asked again = %v", err)
	}
	if _, err := os.Stat(other); err != nil || !hasBranch(t, repo, "task/2-y") {
		t.Fatal("another worktree or branch was touched")
	}
	os.RemoveAll(gone)
	if err := DropScratch(repo, gone, "thread/3-z"); err != nil {
		t.Fatalf("a folder already gone = %v", err)
	}
	if list, _ := runGit(repo, "worktree", "list"); strings.Contains(list, gone) || hasBranch(t, repo, "thread/3-z") {
		t.Fatalf("git still lists the gone folder or its branch:\n%s", list)
	}
	if err := DropScratch(repo, "", ""); err != nil {
		t.Fatalf("nothing to drop = %v", err)
	}
}

// Where the daemon merges there is no "Mark merged": work a person merged
// by hand (a gate-file change, say) is closed by trying the merge again,
// which finds it in the trunk and lands nothing new, without an error.
func TestExecGit_MergeAgainAfterAHandMerge(t *testing.T) {
	repo := gitRepo(t)
	g := ExecGit{}
	wt := filepath.Join(t.TempDir(), "task-1")
	if err := g.AddWorktree(repo, wt, "task/1-x", "main"); err != nil {
		t.Fatal(err)
	}
	os.MkdirAll(filepath.Join(wt, ".claude"), 0o755)
	commitFile(t, wt, ".claude/qa-gates", "a gate")
	if _, err := runGit(repo, "merge", "-q", "--ff-only", "task/1-x"); err != nil { // the person's hand merge
		t.Fatal(err)
	}
	tip, _ := runGit(repo, "rev-parse", "main")
	rev, err := g.Rebase(wt, "main")
	if err != nil || rev != tip {
		t.Fatalf("rebase after a hand merge = %s, %v; want main's tip %s", rev, err, tip)
	}
	if changed, err := g.ChangedUnder(repo, "main", rev, ".claude"); err != nil || len(changed) != 0 {
		t.Fatalf("gate files changed against the trunk after the hand merge = %v, %v", changed, err)
	}
	if err := g.FastForward(repo, rev, "main"); err != nil {
		t.Fatalf("fast-forward to what is already the trunk = %v", err)
	}
}

// Unpushed names the trunk's upstream and counts what it lacks; a trunk
// with none says so, and is no error.
func TestExecGit_Unpushed(t *testing.T) {
	repo := gitRepo(t)
	g := ExecGit{}
	if n, up, err := g.Unpushed(repo, "main"); err != nil || up != "" || n != 0 {
		t.Fatalf("no upstream = %d %q %v", n, up, err)
	}
	remote := t.TempDir()
	for _, args := range [][]string{
		{"-C", remote, "init", "-q", "--bare"},
		{"-C", repo, "remote", "add", "origin", remote},
		{"-C", repo, "push", "-q", "-u", "origin", "main"},
		{"-C", repo, "-c", "user.email=t@t", "-c", "user.name=t", "commit", "-q", "--allow-empty", "-m", "two"},
	} {
		if out, err := exec.Command("git", args...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
	}
	if n, up, err := g.Unpushed(repo, "main"); err != nil || up != "origin/main" || n != 1 {
		t.Fatalf("one ahead = %d %q %v", n, up, err)
	}
}
