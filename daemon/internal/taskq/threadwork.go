package taskq

import (
	"fmt"
	"path/filepath"
)

// A thread with helpers (internal/threads) is a task-queue team a person
// talks to live: its members get their bus names, worktrees and branches by
// the rules a task's parts do, and land through the same merge (land.go).
// This file is those rules under the names a thread uses.

// ThreadBusName is the peers-bus name of a thread's member for repo: the
// twin of BusName, so a member can work out its teammates' names.
func ThreadBusName(repo string, threadID int64) string {
	return fmt.Sprintf("%s-thread-%d", filepath.Base(repo), threadID)
}

// ThreadPlace is where a thread member's worktree goes and what its branch
// is called: beside the repo's task worktrees under root, and the same
// branch name in every repo of the thread.
func ThreadPlace(root, repo string, threadID int64, title string) (worktree, branch string) {
	name := fmt.Sprintf("thread-%d", threadID)
	return filepath.Join(repoDir(root, repo), name), fmt.Sprintf("thread/%d-%s", threadID, slug(title))
}

// MakeWorktree makes path a worktree of repo on branch, cut from the repo's
// trunk when the branch is new, with the repo's own local Claude settings in
// it (the repo's plugins and gates). It returns the trunk. A path that is a
// worktree already is kept as it is.
func MakeWorktree(g Git, repo, path, branch string) (string, error) {
	trunk, err := g.Trunk(repo)
	if err != nil {
		return "", err
	}
	if err := g.AddWorktree(repo, path, branch, trunk); err != nil {
		return "", err
	}
	return trunk, copyLocalSettings(repo, path)
}
