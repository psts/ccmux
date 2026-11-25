// Package wtsweep is the weekly sweep of the worktrees Claude Code leaves
// in a repo's .claude/worktrees/ (a session's Agent tool makes one per
// subagent that runs with worktree isolation, and leaves it when the
// agent changed anything). One that is old, clean and merged is removed;
// any other old one is reported, and the daemon puts it on the repo's
// to-do list. Nothing is forced: a folder git does not know as a worktree
// is left alone, git's own refusals stand, and a branch goes only with
// `git branch -d`.
//
// The git work is a job (kind Kind): removing a worktree with a big
// node_modules takes minutes, and a deploy must not cut it. Every step is
// safe to repeat.
package wtsweep

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"time"

	"ccmux.dev/ccmuxd/internal/jobs"
)

// Kind is the sweep's job kind; its ids are sweep-<hex>.
const (
	Kind    = "worktree-sweep"
	Subject = "worktree-sweep"
	Name    = "sweep"
)

// MinAge is how long a worktree stays untouched before the sweep judges it.
const MinAge = 7 * 24 * time.Hour

// Limit is how long one run of the sweep may take.
const Limit = 2 * time.Hour

// Repo is one repo the sweep looks in: its main folder, and every
// spelling of it a merge or tidy-up may lock under (jobs.LockFile hashes
// the path as written), the main folder among them.
type Repo struct {
	Path  string   `json:"path"`
	Locks []string `json:"locks,omitempty"`
}

// Input is what a sweep is started from. Before is the time (unix ms) a
// worktree last changed before to count as old; it is fixed when the job
// is made, so a run again judges by the same line.
type Input struct {
	Repos  []Repo `json:"repos"`
	Before int64  `json:"before"`
}

// Output is what the sweep did, repo by repo.
type Output struct {
	Repos []RepoResult `json:"repos"`
}

// RepoResult is one repo's sweep. Skipped says why the repo was not
// swept at all (its lock was busy, it has no trunk to judge by).
type RepoResult struct {
	Repo    string    `json:"repo"`
	Skipped string    `json:"skipped,omitempty"`
	Trunk   string    `json:"trunk,omitempty"`
	Removed []Removed `json:"removed,omitempty"`
	Kept    []Kept    `json:"kept,omitempty"`
	// Unknown is the folders under .claude/worktrees git does not know as
	// a worktree of the repo: maybe a person's, so never touched.
	Unknown []string `json:"unknown,omitempty"`
	// Young counts the worktrees changed within MinAge.
	Young int `json:"young,omitempty"`
}

// Removed is a worktree the sweep removed. BranchLeft says why its branch
// stayed ("" when it was deleted, or there was none).
type Removed struct {
	Path       string `json:"path"`
	Branch     string `json:"branch,omitempty"`
	BranchLeft string `json:"branchLeft,omitempty"`
}

// Kept is an old worktree the sweep left, and what keeps it: changes not
// committed, commits the trunk lacks, or git's own reason (Why) when it
// could not be judged or refused the removal.
type Kept struct {
	Path     string `json:"path"`
	Branch   string `json:"branch,omitempty"`
	Dirty    bool   `json:"dirty,omitempty"`
	Unmerged int    `json:"unmerged,omitempty"`
	Why      string `json:"why,omitempty"`
	Bytes    int64  `json:"bytes"`
}

// Run is the sweep's jobs.Runner. Each repo is swept under its lock, taken
// without waiting: a repo a merge or tidy-up works in is skipped this week.
func Run(r *jobs.Run) (any, error) {
	var in Input
	if err := json.Unmarshal(r.Spec.Input, &in); err != nil {
		return nil, fmt.Errorf("the sweep's input is not readable: %w", err)
	}
	s := &sweeper{ctx: r.Ctx, journal: r.Journal, locks: r.Spec.Locks, before: time.UnixMilli(in.Before)}
	var out Output
	for _, repo := range in.Repos {
		if err := r.Ctx.Err(); err != nil {
			return out, err
		}
		out.Repos = append(out.Repos, s.repo(repo))
	}
	return out, nil
}

// repo sweeps one repo once: a repo an earlier run finished is not swept
// again, its result is read back from the journal.
func (s *sweeper) repo(repo Repo) RepoResult {
	key := "repo:" + repo.Path
	var res RepoResult
	if s.journal.Done(key, &res) {
		return res
	}
	locks := append([]string{repo.Path}, repo.Locks...)
	l, err := jobs.TryLockRepos(s.locks, locks)
	if err != nil {
		if errors.Is(err, jobs.ErrBusy) {
			log.Printf("sweep: %s skipped this week: a merge or tidy-up holds it", repo.Path)
			return RepoResult{Repo: repo.Path, Skipped: "a merge or tidy-up held its lock"}
		}
		return RepoResult{Repo: repo.Path, Skipped: "its lock could not be taken: " + err.Error()}
	}
	defer l.Release()
	res = s.sweep(repo.Path)
	if s.ctx.Err() == nil {
		s.record(key, res)
	}
	return res
}
