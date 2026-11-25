package api

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"ccmux.dev/ccmuxd/internal/store"
	"ccmux.dev/ccmuxd/internal/taskq"
)

// mergingJobs stands for a daemon's job manager: a runner holding one
// merges by itself. No method is called in these tests.
type mergingJobs struct{ taskq.Jobs }

// builtTask files a task with one built part in repo, in status.
func builtTask(t *testing.T, s *Server, repo, status string) int64 {
	t.Helper()
	id, err := s.tasks.AddTaskWithParts(store.Task{Title: "t", Status: status, WindowID: "w1"},
		[]store.Part{{RepoPath: repo, Plan: "p", Status: taskq.Ready}})
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func movesOf(t *testing.T, s *Server, id int64) []string {
	t.Helper()
	rec := do(t, s, "GET", "/v1/tasks", "")
	var body struct {
		Tasks []taskView `json:"tasks"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("list = %d %s", rec.Code, rec.Body)
	}
	for _, v := range body.Tasks {
		if v.ID == id {
			return v.Moves
		}
	}
	t.Fatalf("task #%d not listed", id)
	return nil
}

// Where the daemon merges, "Mark merged" is neither offered nor taken: it
// would close the task as done with nothing merged. A daemon with no jobs
// keeps it, as the only way out.
func TestTasks_MarkMergedOnlyWhereNothingMerges(t *testing.T) {
	s := taskServer(t)
	s.taskSettler = &taskq.Runner{Jobs: mergingJobs{}}
	ready := builtTask(t, s, "/p/app", taskq.ReadyToMerge)
	failed := builtTask(t, s, "/p/app", taskq.MergeFailed)
	if got := strings.Join(movesOf(t, s, ready), ","); got != "cancelled" {
		t.Fatalf("moves of a built task where jobs merge = %s, want cancelled only", got)
	}
	if got := strings.Join(movesOf(t, s, failed), ","); got != "ready_to_merge,cancelled" {
		t.Fatalf("moves of a failed merge where jobs merge = %s", got)
	}
	for _, id := range []int64{ready, failed} {
		rec := do(t, s, "PATCH", "/v1/tasks/"+strconv.FormatInt(id, 10), `{"status":"done"}`)
		if rec.Code != 409 || !strings.Contains(rec.Body.String(), "cannot be marked merged by hand: this daemon merges built work itself") {
			t.Fatalf("mark merged #%d = %d %s, want 409 with why", id, rec.Code, rec.Body)
		}
	}
	if got, _ := s.tasks.Task(ready); got.Status != taskq.ReadyToMerge {
		t.Fatalf("a refused mark merged changed the task: %s", got.Status)
	}
	s.taskSettler = &taskq.Runner{} // no jobs
	if got := strings.Join(movesOf(t, s, ready), ","); got != "done,cancelled" {
		t.Fatalf("moves where nothing merges = %s, want done,cancelled", got)
	}
}

// apiGitRepo is a repo with one commit.
func apiGitRepo(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	repo := t.TempDir()
	for _, args := range [][]string{{"init", "-q", "-b", "main"},
		{"-c", "user.email=t@t", "-c", "user.name=t", "commit", "-q", "--allow-empty", "-m", "init"}} {
		if out, err := exec.Command("git", append([]string{"-C", repo}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
	}
	return repo
}

func oldLock(t *testing.T, repo string) string {
	t.Helper()
	lock := filepath.Join(repo, ".git", "index.lock")
	if err := os.WriteFile(lock, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-time.Hour)
	os.Chtimes(lock, old, old)
	return lock
}

// "Remove the stale lock" removes the lock git names in the task's own
// repo, and only that: a lock in a repo the task has no part in, a path in
// the request, and a task not waiting to merge are refused.
func TestTasks_ClearLock(t *testing.T) {
	s := taskServer(t)
	mine, other := apiGitRepo(t), apiGitRepo(t)
	id := builtTask(t, s, mine, taskq.ReadyToMerge)
	otherLock := oldLock(t, other)
	path := "/v1/tasks/" + strconv.FormatInt(id, 10) + "/clear-lock"
	body := `{"path":"` + otherLock + `"}`
	if rec := do(t, s, "POST", path, body); rec.Code != 409 || !strings.Contains(rec.Body.String(), "no git lock file in this task's repos") {
		t.Fatalf("no lock in the task's repo = %d %s", rec.Code, rec.Body)
	}
	if _, err := os.Lstat(otherLock); err != nil {
		t.Fatal("a lock outside the task's repos was removed")
	}
	lock := oldLock(t, mine)
	rec := do(t, s, "POST", path, body)
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), lock) {
		t.Fatalf("clear a stale lock = %d %s", rec.Code, rec.Body)
	}
	if _, err := os.Lstat(lock); err == nil {
		t.Fatal("the stale lock is still there")
	}
	if _, err := os.Lstat(otherLock); err != nil {
		t.Fatal("a lock outside the task's repos was removed")
	}
	running := builtTask(t, s, mine, taskq.Running)
	oldLock(t, mine)
	if rec := do(t, s, "POST", "/v1/tasks/"+strconv.FormatInt(running, 10)+"/clear-lock", ""); rec.Code != 409 || !strings.Contains(rec.Body.String(), "only for a built task waiting to merge") {
		t.Fatalf("clear lock on a running task = %d %s", rec.Code, rec.Body)
	}
}

// The sidebar's count: built features whose merge waits on a folder long
// enough to ask a person, per window, beside the statuses.
func TestTaskCounts_CountStuckMerges(t *testing.T) {
	tasks := []store.Task{{ID: 1, WindowID: "w1", Status: taskq.ReadyToMerge}, {ID: 2, WindowID: "w1", Status: taskq.ReadyToMerge}, {ID: 3, WindowID: "w2", Status: taskq.Running}}
	views := []taskView{
		{Task: tasks[0], Block: &taskq.MergeBlock{Kind: taskq.BlockLock, NeedsPerson: true}},
		{Task: tasks[1], Block: &taskq.MergeBlock{Kind: taskq.BlockLock}},
		{Task: tasks[2]},
	}
	got := taskCounts(tasks, views)
	if got["w1"][stuckKey] != 1 || got["w2"][stuckKey] != 0 || got["w1"][taskq.ReadyToMerge] != 2 {
		t.Fatalf("counts = %v", got)
	}
}
