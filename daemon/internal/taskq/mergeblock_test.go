package taskq

import (
	"testing"
	"time"
)

// A built feature held by a git lock carries the block, from the lock's own
// time; once it is StaleLockAge old it asks a person and may be removed.
func TestMergeBlock_ALockAsksAPersonAfterTenMinutes(t *testing.T) {
	g := newRig(t)
	g.feature(1, 0, "app")
	g.tick()
	g.doneParts(1)
	lockAt := g.now.Add(-5 * time.Minute)
	g.git.folders = map[string]FolderBlock{"/p/app": {Kind: BlockLock, Repo: "/p/app", Lock: "/p/app/.git/index.lock", LockTime: lockAt,
		Why: "git is busy in app (/p/app/.git/index.lock exists)"}}
	g.tick()
	if got := g.st.task(1); got.Status != ReadyToMerge {
		t.Fatalf("a locked repo = %s, want ready to merge", got.Status)
	}
	b, ok := g.r.BlockOf(1)
	if !ok || b.Kind != BlockLock || b.Repo != "app" || b.Since != lockAt.UnixMilli() || b.NeedsPerson || b.Clearable {
		t.Fatalf("a five-minute lock = %+v %v, want a lock block from its time, not asking yet", b, ok)
	}
	g.now = g.now.Add(6 * time.Minute)
	g.tick()
	b, _ = g.r.BlockOf(1)
	if !b.NeedsPerson || !b.Clearable {
		t.Fatalf("an eleven-minute lock = %+v, want it to ask a person and be removable", b)
	}
	delete(g.git.folders, "/p/app")
	g.tick()
	if _, ok := g.r.BlockOf(1); ok {
		t.Fatal("a block outlived the lock")
	}
	if g.st.task(1).Status != Done {
		t.Fatalf("lock gone = %s, want merged", g.st.task(1).Status)
	}
}

// The other folder blocks count from when the runner first saw them, keep
// that time while they last, and are never removable.
func TestMergeBlock_OtherBlocksCountFromFirstSeen(t *testing.T) {
	g := newRig(t)
	g.feature(1, 0, "app")
	g.tick()
	g.doneParts(1)
	g.git.blocked["/p/app"] = "app has uncommitted changes on dev"
	first := g.now
	g.tick()
	g.now = g.now.Add(9 * time.Minute)
	g.tick()
	b, ok := g.r.BlockOf(1)
	if !ok || b.Kind != BlockUncommitted || b.Since != first.UnixMilli() || b.NeedsPerson {
		t.Fatalf("nine minutes of a dirty folder = %+v %v", b, ok)
	}
	g.now = g.now.Add(2 * time.Minute)
	g.tick()
	if b, _ = g.r.BlockOf(1); !b.NeedsPerson || b.Clearable {
		t.Fatalf("eleven minutes of a dirty folder = %+v, want it to ask a person, nothing to remove", b)
	}
	task := g.st.task(1)
	task.Status = Cancelled
	g.st.SaveTask(task)
	g.tick()
	if _, ok := g.r.BlockOf(1); ok {
		t.Fatal("a cancelled task kept its block")
	}
}
