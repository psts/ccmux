package wtsweep

import (
	"encoding/json"
	"fmt"
	"log"
	"path/filepath"
	"strings"

	"ccmux.dev/ccmuxd/internal/jobs"
	"ccmux.dev/ccmuxd/internal/store"
)

// By is who the sweep's to-dos are added by.
const By = "weekly sweep"

// Todo files one item on repo's list unless an unfinished one starts with
// prefix (api.Server.AddTodoOnce).
type Todo func(repo, prefix, text, urgency string) error

// Handler is the daemon's side of the sweep: it files a to-do for each
// worktree the sweep kept. Applied is the job's applied journal
// (jobs.Manager.Applied), so a handler called again after a crash files
// nothing twice; the to-do's own prefix keeps a worktree one item from
// week to week.
type Handler struct {
	Todo    Todo
	Applied func(id string) (*jobs.Journal, error)
}

// Apply files the to-dos of a sweep's result. A result that cannot be
// read is logged and settled: there is nothing to file, and next week's
// sweep looks again. A to-do that could not be filed is an error, so the
// handler is called again.
func (h Handler) Apply(job store.Job, res jobs.Result) error {
	if res.Error != "" {
		log.Printf("sweep: %s did not run to its end: %s", job.ID, res.Error)
	}
	var out Output
	if len(res.Output) == 0 || json.Unmarshal(res.Output, &out) != nil {
		log.Printf("sweep: %s has no result to read; nothing filed", job.ID)
		return nil
	}
	applied, err := h.Applied(job.ID)
	if err != nil {
		return fmt.Errorf("sweep %s: the record of what was filed: %w", job.ID, err)
	}
	for _, repo := range out.Repos {
		logRepo(repo)
		for _, k := range repo.Kept {
			if err := h.file(applied, repo, k); err != nil {
				return err
			}
		}
	}
	return nil
}

// file puts one kept worktree on its repo's list, once.
func (h Handler) file(applied *jobs.Journal, repo RepoResult, k Kept) error {
	key := "todo:" + k.Path
	if applied.Done(key, nil) {
		return nil
	}
	prefix, text := TodoText(repo, k)
	if err := h.Todo(repo.Repo, prefix, text, urgency(k)); err != nil {
		return fmt.Errorf("sweep: the to-do for %s: %w", k.Path, err)
	}
	if err := applied.Record(key, nil); err != nil {
		log.Printf("sweep: that the to-do for %s was filed is not written down; filing again finds it: %v", k.Path, err)
	}
	return nil
}

// TodoText is a kept worktree's to-do, and the start of it that names the
// worktree: the same worktree next week, with other numbers, is the same
// item.
func TodoText(repo RepoResult, k Kept) (prefix, text string) {
	prefix = fmt.Sprintf("Leftover Claude Code worktree %s (", k.Path)
	branch := k.Branch
	if branch == "" {
		branch = "detached"
	}
	text = fmt.Sprintf("%sbranch %s, %s) is kept because %s. Merge or remove it by hand (git worktree remove).",
		prefix, branch, gigabytes(k.Bytes), why(repo, k))
	return prefix, text
}

func why(repo RepoResult, k Kept) string {
	var reasons []string
	if k.Dirty {
		reasons = append(reasons, "it has changes not committed")
	}
	if k.Unmerged > 0 {
		reasons = append(reasons, fmt.Sprintf("%d commit%s not on %s", k.Unmerged, plural(k.Unmerged), repo.Trunk))
	}
	if k.Why != "" {
		reasons = append(reasons, "git said: "+firstLine(k.Why))
	}
	if len(reasons) == 0 {
		return "the sweep could not tell"
	}
	return strings.Join(reasons, "; ")
}

// urgency: a big leftover is worth a look sooner.
func urgency(k Kept) string {
	if k.Bytes >= 1_000_000_000 {
		return "medium"
	}
	return "low"
}

func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}

// firstLine is the first line of git's words, cut to fit on a to-do (an
// item is at most 600 characters).
func firstLine(text string) string {
	line, _, _ := strings.Cut(text, "\n")
	if r := []rune(line); len(r) > 200 {
		line = string(r[:200]) + "…"
	}
	return line
}

func gigabytes(n int64) string {
	if n < 1_000_000 {
		return "under 1 MB"
	}
	if n < 1_000_000_000 {
		return fmt.Sprintf("%d MB", n/1_000_000)
	}
	return fmt.Sprintf("%.1f GB", float64(n)/1e9)
}

// logRepo says what the sweep did in one repo, in the daemon's log.
func logRepo(r RepoResult) {
	name := filepath.Base(r.Repo)
	if r.Skipped != "" {
		log.Printf("sweep: %s skipped: %s", name, r.Skipped)
		return
	}
	if len(r.Removed)+len(r.Kept)+len(r.Unknown)+r.Young == 0 {
		return
	}
	log.Printf("sweep: %s: %d removed, %d kept, %d younger than a week, %d not git's", name, len(r.Removed), len(r.Kept), r.Young, len(r.Unknown))
}

// Lost is a sweep that ended with no result: logged, and next week's
// looks again. Nothing is half done in a way a person must finish: every
// step is git's own.
func (h Handler) Lost(job store.Job, cause error) error {
	log.Printf("sweep: %s ended with no result: %v", job.ID, cause)
	return nil
}

// Stopped is a sweep a person stopped.
func (h Handler) Stopped(job store.Job) error {
	log.Printf("sweep: %s was stopped", job.ID)
	return nil
}
