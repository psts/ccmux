package wtsweep

import (
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"time"

	"ccmux.dev/ccmuxd/internal/jobs"
)

// Folder is where Claude Code puts a repo's subagent worktrees.
func Folder(repo string) string { return filepath.Join(repo, ".claude", "worktrees") }

// sweeper is one run of the sweep.
type sweeper struct {
	ctx     context.Context
	journal *jobs.Journal
	locks   string
	before  time.Time
}

// begin writes down that a step starts.
func (s *sweeper) begin(key, label string) {
	if err := s.journal.Begin(key, label); err != nil {
		log.Printf("sweep: journal: %v", err)
	}
}

// record writes a finished step down. A journal that cannot be written
// costs a later run time, never the work: every step is safe to repeat.
func (s *sweeper) record(key string, data any) {
	if err := s.journal.Record(key, data); err != nil {
		log.Printf("sweep: journal: %v", err)
	}
}

// sweep judges every folder in the repo's .claude/worktrees.
func (s *sweeper) sweep(repo string) RepoResult {
	res := RepoResult{Repo: repo, Removed: s.removedBefore(repo)}
	entries, err := os.ReadDir(Folder(repo))
	if errors.Is(err, fs.ErrNotExist) || (err == nil && len(entries) == 0) {
		return res // no folder, nothing to sweep
	}
	if err != nil {
		log.Printf("sweep: %s skipped: %v", repo, err)
		res.Skipped = "its .claude/worktrees could not be read: " + err.Error()
		return res
	}
	if res.Trunk, err = trunk(s.ctx, repo); err != nil {
		log.Printf("sweep: %s skipped: %v", repo, err)
		res.Skipped = err.Error()
		return res
	}
	known, err := worktrees(s.ctx, repo)
	if err != nil {
		res.Skipped = "its worktrees could not be listed: " + err.Error()
		return res
	}
	dir := realPath(Folder(repo))
	for _, e := range entries {
		path := filepath.Join(dir, e.Name())
		wt, ok := known[path]
		// Only a real folder is a candidate: a link there to another of
		// the repo's worktrees would otherwise be taken for that one.
		if !ok || !e.IsDir() {
			log.Printf("sweep: %s is not a worktree git knows; left alone", path)
			res.Unknown = append(res.Unknown, path)
			continue
		}
		s.judge(&res, wt, known)
	}
	return res
}

// removedBefore is what an earlier run of this job removed in repo: the
// folders are gone, so only the journal still knows.
func (s *sweeper) removedBefore(repo string) []Removed {
	var out []Removed
	for _, raw := range s.journal.Recorded(removedKey(repo, "")) {
		var r Removed
		if json.Unmarshal(raw, &r) == nil {
			out = append(out, r)
		}
	}
	return out
}

func removedKey(repo, path string) string { return "removed:" + repo + "|" + path }

// removingKey is the step of one removal, begun before git is asked and
// ended whatever git said: a step that began and never ended is a
// removal the job was killed in.
func removingKey(path string) string { return "removing:" + path }

// judge removes one worktree when it is old, clean and merged, and
// otherwise writes down what keeps it.
func (s *sweeper) judge(res *RepoResult, wt worktree, known map[string]worktree) {
	cut := s.journal.Cut(removingKey(wt.Path))
	if !cut && lastChange(wt.Path).After(s.before) {
		res.Young++
		return
	}
	kept, removable := s.keeps(res, wt, cut)
	if removable {
		removed, why := s.remove(res.Repo, wt, known, cut)
		if why == "" {
			res.Removed = append(res.Removed, removed)
			return
		}
		kept.Why = why
	}
	kept.Bytes = size(wt.Path)
	res.Kept = append(res.Kept, kept)
}

// keeps is what keeps a worktree, and whether nothing does. A removal an
// earlier run was killed in is finished: the worktree was clean when it
// began, and what it looks like now (files half gone) is the removal's
// own doing. Its commits are counted again all the same.
func (s *sweeper) keeps(res *RepoResult, wt worktree, cut bool) (Kept, bool) {
	kept := Kept{Path: wt.Path, Branch: wt.Branch}
	var err error
	if kept.Unmerged, err = unmerged(s.ctx, res.Repo, res.Trunk, wt.Head); err != nil {
		kept.Why = err.Error()
		return kept, false
	}
	if cut {
		return kept, kept.Unmerged == 0
	}
	if kept.Dirty, err = dirty(s.ctx, wt.Path); err != nil {
		kept.Why = err.Error()
		return kept, false
	}
	return kept, !kept.Dirty && kept.Unmerged == 0
}

// remove removes a clean, merged worktree the way git does it, never by
// deleting its folder, and then its branch when it is merged and no other
// worktree has it out. A refusal of git's is the reason it stays. finish
// is a removal an earlier run began and was killed in: git is told to
// remove what is left of it (--force), since git refuses a worktree with
// files missing.
func (s *sweeper) remove(repo string, wt worktree, known map[string]worktree, finish bool) (Removed, string) {
	args := []string{"worktree", "remove", wt.Path}
	if finish {
		args = []string{"worktree", "remove", "--force", wt.Path}
	}
	s.begin(removingKey(wt.Path), "removing "+wt.Path)
	_, err := git(s.ctx, repo, args...)
	if s.ctx.Err() != nil {
		return Removed{}, "the sweep ran out of time while removing it"
	}
	s.record(removingKey(wt.Path), nil)
	if err != nil {
		log.Printf("sweep: %s stays: %v", wt.Path, err)
		return Removed{}, err.Error()
	}
	if _, err := git(s.ctx, repo, "worktree", "prune"); err != nil {
		log.Printf("sweep: %s: prune: %v", repo, err)
	}
	r := Removed{Path: wt.Path, Branch: wt.Branch}
	if r.Branch != "" {
		r.BranchLeft = s.dropBranch(repo, wt, known)
	}
	log.Printf("sweep: removed %s (branch %q%s)", wt.Path, r.Branch, note(r.BranchLeft))
	s.record(removedKey(repo, wt.Path), r)
	return r, ""
}

// dropBranch deletes a removed worktree's branch with `git branch -d`,
// which refuses one that is not merged. It returns why the branch stays,
// "" when it is gone.
func (s *sweeper) dropBranch(repo string, wt worktree, known map[string]worktree) string {
	for path, other := range known {
		if path != realPath(wt.Path) && other.Branch == wt.Branch {
			return "another worktree has it checked out: " + other.Path
		}
	}
	if _, err := git(s.ctx, repo, "branch", "-d", wt.Branch); err != nil {
		return err.Error()
	}
	return ""
}

func note(text string) string {
	if text == "" {
		return ""
	}
	return "; the branch stays: " + text
}

// lastChange is the newest change of a worktree's folder, of what is at
// its top level, and of its .git file (which is at its top level). A
// worktree is never walked whole for this: a node_modules is large.
func lastChange(dir string) time.Time {
	var newest time.Time
	if fi, err := os.Lstat(dir); err == nil {
		newest = fi.ModTime()
	}
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if fi, err := e.Info(); err == nil && fi.ModTime().After(newest) {
			newest = fi.ModTime()
		}
	}
	return newest
}

// size is the bytes of the files under dir, links not followed. What
// cannot be read counts nothing.
func size(dir string) int64 {
	var total int64
	filepath.WalkDir(dir, func(_ string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.Type().IsRegular() {
			if fi, err := d.Info(); err == nil {
				total += fi.Size()
			}
		}
		return nil
	})
	return total
}
