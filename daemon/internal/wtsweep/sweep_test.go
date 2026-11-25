package wtsweep

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"ccmux.dev/ccmuxd/internal/jobs"
	"ccmux.dev/ccmuxd/internal/store"
)

func run(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir, "-c", "user.email=t@t", "-c", "user.name=t"}, args...)...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v %s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// gitRepo is a repo with one commit on branch.
func gitRepo(t *testing.T, branch string) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	repo := t.TempDir()
	run(t, repo, "init", "-q", "-b", branch)
	run(t, repo, "commit", "-q", "--allow-empty", "-m", "init")
	return repo
}

// worktreeAt adds a worktree of repo at path on a new branch.
func worktreeAt(t *testing.T, repo, path, branch string) string {
	t.Helper()
	run(t, repo, "worktree", "add", "-q", "-b", branch, path)
	return path
}

// subagent is a worktree where Claude Code puts one.
func subagent(t *testing.T, repo, name string) string {
	t.Helper()
	return worktreeAt(t, repo, filepath.Join(Folder(repo), name), "worktree-"+name)
}

// age sets a worktree's folder and its top level back to when.
func age(t *testing.T, dir string, when time.Time) {
	t.Helper()
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if err := os.Chtimes(filepath.Join(dir, e.Name()), when, when); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Chtimes(dir, when, when); err != nil {
		t.Fatal(err)
	}
}

func exists(path string) bool { _, err := os.Lstat(path); return err == nil }

func hasBranch(repo, branch string) bool {
	return exec.Command("git", "-C", repo, "rev-parse", "--verify", "--quiet", "refs/heads/"+branch).Run() == nil
}

// sweepOnce runs the sweep job as job-exec would, over a journal in jobDir.
func sweepOnce(t *testing.T, jobDir, locks string, in Input) Output {
	t.Helper()
	raw, _ := json.Marshal(in)
	journal, err := jobs.OpenJournal(filepath.Join(jobDir, "journal.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	out, err := Run(&jobs.Run{Spec: jobs.Spec{ID: "sweep-t", Kind: Kind, Input: raw, Locks: locks}, Journal: journal, Ctx: context.Background()})
	if err != nil {
		t.Fatal(err)
	}
	return out.(Output)
}

// The sweep's rule, over real git: an old worktree that is clean and
// merged is removed with its merged branch; a dirty one, an unmerged one
// and a young one stay, and so does a folder git does not know, and a
// link to a worktree that would itself qualify.
func TestTheSweepRemovesOnlyOldCleanMergedWorktrees(t *testing.T) {
	repo := gitRepo(t, "main")
	old := time.Now().Add(-10 * 24 * time.Hour)
	done := subagent(t, repo, "done")
	dirtyWT := subagent(t, repo, "dirty")
	os.WriteFile(filepath.Join(dirtyWT, "notes.txt"), []byte("half"), 0o644)
	ahead := subagent(t, repo, "ahead")
	run(t, ahead, "commit", "-q", "--allow-empty", "-m", "work")
	young := subagent(t, repo, "young")
	stray := filepath.Join(Folder(repo), "stray")
	os.MkdirAll(stray, 0o755)
	elsewhere := worktreeAt(t, repo, filepath.Join(t.TempDir(), "elsewhere"), "elsewhere")
	link := filepath.Join(Folder(repo), "link")
	if err := os.Symlink(elsewhere, link); err != nil {
		t.Fatal(err)
	}
	for _, wt := range []string{done, dirtyWT, ahead, stray, elsewhere} {
		age(t, wt, old)
	}

	out := sweepOnce(t, t.TempDir(), t.TempDir(), Input{Repos: []Repo{{Path: repo}}, Before: time.Now().Add(-MinAge).UnixMilli()})
	if len(out.Repos) != 1 {
		t.Fatalf("out = %+v", out)
	}
	res := out.Repos[0]
	if res.Skipped != "" || res.Trunk != "main" {
		t.Fatalf("swept against main: %+v", res)
	}
	if exists(done) || hasBranch(repo, "worktree-done") {
		t.Fatal("the old, clean, merged worktree and its merged branch are gone")
	}
	if len(res.Removed) != 1 || res.Removed[0].Branch != "worktree-done" || res.Removed[0].BranchLeft != "" {
		t.Fatalf("removed = %+v", res.Removed)
	}
	for _, kept := range []string{dirtyWT, ahead, young, stray, elsewhere, link} {
		if !exists(kept) {
			t.Fatalf("%s stays", kept)
		}
	}
	if res.Young != 1 || len(res.Unknown) != 2 {
		t.Fatalf("one young, two not git's (the stray folder and the link): %+v", res)
	}
	if len(res.Kept) != 2 {
		t.Fatalf("kept = %+v", res.Kept)
	}
	byPath := map[string]Kept{}
	for _, k := range res.Kept {
		byPath[filepath.Base(k.Path)] = k
	}
	if k := byPath["dirty"]; !k.Dirty || k.Unmerged != 0 || k.Bytes == 0 {
		t.Fatalf("dirty = %+v", k)
	}
	if k := byPath["ahead"]; k.Dirty || k.Unmerged != 1 || k.Branch != "worktree-ahead" {
		t.Fatalf("ahead = %+v", k)
	}
}

// Run again (the job may be), the sweep says what it did the first time,
// though the folders it removed are gone now.
func TestASweepRunAgainReportsTheSame(t *testing.T) {
	repo := gitRepo(t, "main")
	done := subagent(t, repo, "done")
	age(t, done, time.Now().Add(-8*24*time.Hour))
	jobDir, locks := t.TempDir(), t.TempDir()
	in := Input{Repos: []Repo{{Path: repo}}, Before: time.Now().Add(-MinAge).UnixMilli()}
	first := sweepOnce(t, jobDir, locks, in)
	again := sweepOnce(t, jobDir, locks, in)
	if len(first.Repos[0].Removed) != 1 || len(again.Repos[0].Removed) != 1 {
		t.Fatalf("first %+v, again %+v", first, again)
	}
}

// A removal the job was killed in is finished on the next run, though the
// folder is half gone (and so looks dirty, and young).
func TestARemovalCutOffHalfWayIsFinished(t *testing.T) {
	repo := gitRepo(t, "main")
	os.WriteFile(filepath.Join(repo, "a.txt"), []byte("a"), 0o644)
	run(t, repo, "add", "a.txt")
	run(t, repo, "commit", "-q", "-m", "a")
	done := subagent(t, repo, "done")
	os.Remove(filepath.Join(done, "a.txt")) // what a killed removal leaves
	jobDir := t.TempDir()
	journal, _ := jobs.OpenJournal(filepath.Join(jobDir, "journal.jsonl"))
	journal.Begin(removingKey(done), "removing")
	out := sweepOnce(t, jobDir, t.TempDir(), Input{Repos: []Repo{{Path: repo}}, Before: time.Now().Add(-MinAge).UnixMilli()})
	if exists(done) || len(out.Repos[0].Removed) != 1 {
		t.Fatalf("the cut-off removal is finished: %+v", out)
	}
}

// A repo with no trunk git can name is not swept; nor is one whose lock a
// merge holds, and a busy one is not written down as done, so a run again
// tries it.
func TestReposTheSweepSkips(t *testing.T) {
	noTrunk := gitRepo(t, "trunk")
	wt := subagent(t, noTrunk, "x")
	age(t, wt, time.Now().Add(-30*24*time.Hour))
	busy := gitRepo(t, "main")
	busyWT := subagent(t, busy, "y")
	age(t, busyWT, time.Now().Add(-30*24*time.Hour))
	locks, jobDir := t.TempDir(), t.TempDir()
	held, err := jobs.LockRepos(context.Background(), locks, []string{busy + "/"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	in := Input{Repos: []Repo{{Path: noTrunk}, {Path: busy, Locks: []string{busy + "/"}}}, Before: time.Now().Add(-MinAge).UnixMilli()}
	out := sweepOnce(t, jobDir, locks, in)
	if !strings.Contains(out.Repos[0].Skipped, "default branch") || !exists(wt) {
		t.Fatalf("no trunk: %+v", out.Repos[0])
	}
	if !strings.Contains(out.Repos[1].Skipped, "lock") || !exists(busyWT) {
		t.Fatalf("busy: %+v", out.Repos[1])
	}
	held.Release()
	if again := sweepOnce(t, jobDir, locks, in); len(again.Repos[1].Removed) != 1 || exists(busyWT) {
		t.Fatalf("once free, the busy repo is swept: %+v", again.Repos[1])
	}
}

// A branch another worktree has out stays when its worktree goes.
func TestABranchCheckedOutElsewhereStays(t *testing.T) {
	repo := gitRepo(t, "main")
	done := subagent(t, repo, "done")
	run(t, done, "checkout", "-q", "--detach")
	other := worktreeAt(t, repo, filepath.Join(t.TempDir(), "other"), "other")
	run(t, other, "checkout", "-q", "worktree-done")
	run(t, done, "checkout", "-q", "-B", "worktree-done-2")
	known := map[string]worktree{realPath(other): {Path: other, Branch: "worktree-done"}}
	s := &sweeper{ctx: context.Background()}
	if why := s.dropBranch(repo, worktree{Path: done, Branch: "worktree-done"}, known); !strings.Contains(why, "another worktree") || !hasBranch(repo, "worktree-done") {
		t.Fatalf("why = %q", why)
	}
}

type todoCall struct{ repo, prefix, text, urgency string }

// The handler files one to-do per kept worktree, and none a second time
// when it is called again on the same job.
func TestTheHandlerFilesEachKeptWorktreeOnce(t *testing.T) {
	jobDir := t.TempDir()
	var calls []todoCall
	h := Handler{
		Todo: func(repo, prefix, text, urgency string) error {
			calls = append(calls, todoCall{repo, prefix, text, urgency})
			return nil
		},
		Applied: func(string) (*jobs.Journal, error) { return jobs.OpenJournal(filepath.Join(jobDir, "applied.jsonl")) },
	}
	out := Output{Repos: []RepoResult{{
		Repo: "/p/app", Trunk: "main",
		Kept: []Kept{
			{Path: "/p/app/.claude/worktrees/a", Branch: "worktree-a", Dirty: true, Bytes: 2_500_000_000},
			{Path: "/p/app/.claude/worktrees/b", Branch: "worktree-b", Unmerged: 3, Bytes: 40_000_000},
		},
		Removed: []Removed{{Path: "/p/app/.claude/worktrees/c"}},
	}}}
	raw, _ := json.Marshal(out)
	res := jobs.Result{Output: raw}
	job := store.Job{ID: "sweep-abc"}
	for i := 0; i < 2; i++ {
		if err := h.Apply(job, res); err != nil {
			t.Fatal(err)
		}
	}
	if len(calls) != 2 {
		t.Fatalf("two kept worktrees, two to-dos, once: %+v", calls)
	}
	a, b := calls[0], calls[1]
	if a.repo != "/p/app" || !strings.HasPrefix(a.text, a.prefix) || !strings.Contains(a.text, "changes not committed") ||
		!strings.Contains(a.text, "2.5 GB") || a.urgency != "medium" {
		t.Fatalf("a = %+v", a)
	}
	if !strings.Contains(b.text, "3 commits not on main") || !strings.Contains(b.text, "worktree-b") || b.urgency != "low" {
		t.Fatalf("b = %+v", b)
	}
	if err := h.Apply(job, jobs.Result{Error: "ran out of time"}); err != nil {
		t.Fatalf("a result with nothing to read is settled, not retried: %v", err)
	}
	if h.Lost(job, os.ErrNotExist) != nil || h.Stopped(job) != nil {
		t.Fatal("lost and stopped are settled")
	}
}

// A removal git refuses (a locked worktree) is a worktree kept, with
// git's reason, and nothing is forced.
func TestARemovalGitRefusesIsKeptWithItsReason(t *testing.T) {
	repo := gitRepo(t, "main")
	locked := subagent(t, repo, "locked")
	run(t, repo, "worktree", "lock", "--reason", "a person's", locked)
	age(t, locked, time.Now().Add(-8*24*time.Hour))
	out := sweepOnce(t, t.TempDir(), t.TempDir(), Input{Repos: []Repo{{Path: repo}}, Before: time.Now().Add(-MinAge).UnixMilli()})
	kept := out.Repos[0].Kept
	if !exists(locked) || len(kept) != 1 || !strings.Contains(kept[0].Why, "locked") {
		t.Fatalf("kept = %+v", kept)
	}
	if _, text := TodoText(out.Repos[0], kept[0]); !strings.Contains(text, "git said") {
		t.Fatalf("the to-do says git's reason: %s", text)
	}
}

// A .claude/worktrees that cannot be read is a repo skipped, with why,
// not one swept clean; a repo with no such folder is simply empty.
func TestAnUnreadableWorktreesFolderIsSkippedWithItsReason(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("root reads every folder")
	}
	repo := gitRepo(t, "main")
	wt := subagent(t, repo, "x")
	age(t, wt, time.Now().Add(-30*24*time.Hour))
	if err := os.Chmod(Folder(repo), 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(Folder(repo), 0o755) })
	none := gitRepo(t, "main")
	out := sweepOnce(t, t.TempDir(), t.TempDir(), Input{Repos: []Repo{{Path: repo}, {Path: none}}, Before: time.Now().Add(-MinAge).UnixMilli()})
	if !strings.Contains(out.Repos[0].Skipped, "could not be read") {
		t.Fatalf("unreadable: %+v", out.Repos[0])
	}
	if out.Repos[1].Skipped != "" || len(out.Repos[1].Kept)+len(out.Repos[1].Removed) != 0 {
		t.Fatalf("no folder, nothing to sweep and nothing skipped: %+v", out.Repos[1])
	}
}

// One to-do that cannot be filed does not hold the job open (an open job
// blocks every later sweep): the others are filed, the job settles, and a
// later run files the one that failed, never one twice.
func TestAToDoThatCannotBeFiledDoesNotStopTheSweep(t *testing.T) {
	jobDir := t.TempDir()
	failA := true
	var filed []string
	h := Handler{
		Todo: func(repo, prefix, text, urgency string) error {
			if failA && strings.Contains(prefix, "/a (") {
				return errors.New("store busy")
			}
			filed = append(filed, prefix)
			return nil
		},
		Applied: func(string) (*jobs.Journal, error) { return jobs.OpenJournal(filepath.Join(jobDir, "applied.jsonl")) },
	}
	out := Output{Repos: []RepoResult{{Repo: "/p/app", Trunk: "main", Kept: []Kept{
		{Path: "/p/app/.claude/worktrees/a", Branch: "wa", Dirty: true},
		{Path: "/p/app/.claude/worktrees/b", Branch: "wb", Unmerged: 1},
	}}}}
	raw, _ := json.Marshal(out)
	job := store.Job{ID: "sweep-x"}
	if err := h.Apply(job, jobs.Result{Output: raw}); err != nil {
		t.Fatalf("the job settles: %v", err)
	}
	if len(filed) != 1 || !strings.Contains(filed[0], "/b (") {
		t.Fatalf("b is filed though a failed: %v", filed)
	}
	failA = false
	if err := h.Apply(job, jobs.Result{Output: raw}); err != nil {
		t.Fatal(err)
	}
	if len(filed) != 2 || !strings.Contains(filed[1], "/a (") {
		t.Fatalf("a is filed on a later run, b not again: %v", filed)
	}
}
