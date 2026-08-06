// Package gitstatus computes a repository's sidebar-dashboard status — branch,
// tracking ahead/behind, default-branch comparison, changed files — by shelling
// out to git. It is a Go port of the native app's local GitService: hosted
// workspaces render the SAME dashboard, but from daemon-side data, because the
// repo lives on the daemon's host (which may be a remote server).
package gitstatus

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

// maxFilesPerBucket bounds each changed-file list on the wire. A dashboard with
// hundreds of entries is unreadable anyway; the counts stay exact via the lists
// the lens shows, and a mega-refactor doesn't bloat every /v1/workspaces poll.
const maxFilesPerBucket = 100

// File is one changed path. Status letters mirror the app's GitStatusInfo
// (`M` modified, `A` added, `D` deleted, `R` renamed, `?` untracked).
type File struct {
	Path   string `json:"path"`
	Status string `json:"status"`
}

// Status mirrors the app's GitStatusInfo — field names match its decoder 1:1.
type Status struct {
	IsGitRepo      bool   `json:"isGitRepo"`
	Branch         string `json:"branch,omitempty"`
	TrackingBranch string `json:"trackingBranch,omitempty"`
	Ahead          int    `json:"ahead,omitempty"`
	Behind         int    `json:"behind,omitempty"`
	DefaultBranch  string `json:"defaultBranch,omitempty"`
	AheadOfDefault int    `json:"aheadOfDefault,omitempty"`
	BehindDefault  int    `json:"behindDefault,omitempty"`
	StagedFiles    []File `json:"stagedFiles,omitempty"`
	ModifiedFiles  []File `json:"modifiedFiles,omitempty"`
	DeletedFiles   []File `json:"deletedFiles,omitempty"`
	UntrackedFiles []File `json:"untrackedFiles,omitempty"`
}

// Full computes the dashboard status for repoPath in one `git status` call plus
// (off the default branch) one `rev-list` for the "vs default ↑↓" numbers.
// defaultBranch is the cached comparison base from DetectDefaultBranch ("" =
// none/unresolved). A non-repo returns {IsGitRepo:false}; a transient exec
// failure returns an error so the caller keeps its previous status.
func Full(ctx context.Context, repoPath, defaultBranch string) (*Status, error) {
	out, code, err := run(ctx, repoPath, "status", "--porcelain=v2", "--branch")
	if code == 128 {
		return &Status{}, nil // genuinely not a git repo
	}
	if err != nil {
		return nil, err
	}
	if code != 0 {
		return nil, fmt.Errorf("git status exit %d", code)
	}
	st := Parse(out)
	st.IsGitRepo = true
	// defaultBranch may be a remote-tracking ref ("origin/main") — count with
	// the ref, but display (and compare the current branch against) the short
	// name, so the sidebar reads "vs main" either way.
	shortDefault := strings.TrimPrefix(defaultBranch, "origin/")
	st.DefaultBranch = shortDefault
	if defaultBranch != "" && st.Branch != shortDefault {
		if out, code, _ := run(ctx, repoPath, "rev-list", "--count", "--left-right", defaultBranch+"...HEAD"); code == 0 {
			fields := strings.Fields(out)
			if len(fields) == 2 {
				st.BehindDefault, _ = strconv.Atoi(fields[0])
				st.AheadOfDefault, _ = strconv.Atoi(fields[1])
			}
		}
	}
	return st, nil
}

// DetectDefaultBranch resolves the ref to compare against — preferring local
// `main`/`master`, then remote-tracking `origin/main`/`origin/master`, then
// the remote default (origin/HEAD). The trunk names come first on purpose: a
// repo can point origin/HEAD at an integration branch like `dev` that is
// worked on directly; the comparison the dashboard wants is still "vs the
// release trunk". The remote-tracking step exists for fresh clones checked
// out on that integration branch — they have no local main, and falling
// straight to origin/HEAD would compare dev vs dev and hide the row. Call
// once per repo and cache ("" = no trunk). The result is a REF (possibly
// "origin/main"); Full derives the display name from it.
func DetectDefaultBranch(ctx context.Context, repoPath string) string {
	for _, name := range []string{"main", "master"} {
		if out, code, _ := run(ctx, repoPath, "rev-parse", "--verify", "refs/heads/"+name); code == 0 && strings.TrimSpace(out) != "" {
			return name
		}
	}
	for _, name := range []string{"origin/main", "origin/master"} {
		if out, code, _ := run(ctx, repoPath, "rev-parse", "--verify", "refs/remotes/"+name); code == 0 && strings.TrimSpace(out) != "" {
			return name
		}
	}
	out, code, _ := run(ctx, repoPath, "symbolic-ref", "--short", "refs/remotes/origin/HEAD")
	if code == 0 {
		if s := strings.TrimSpace(out); s != "" {
			parts := strings.Split(s, "/") // "origin/dev" → "dev"
			return parts[len(parts)-1]
		}
	}
	return ""
}

// Parse turns `git status --porcelain=v2 --branch` output into a Status
// (without IsGitRepo/default-branch fields, which Full fills in). Pure function
// — a direct port of the app's GitService.parseStatusV2.
func Parse(out string) *Status {
	st := &Status{}
	oid, head := "", ""
	for _, line := range strings.Split(out, "\n") {
		if line == "" {
			continue
		}
		switch line[0] {
		case '#':
			parseHeader(line, &oid, &head, st)
		case '1':
			if len(line) > 3 {
				categorize(line[2], line[3], lastField(line, 9), st)
			}
		case '2':
			if len(line) > 3 {
				raw := lastField(line, 10)
				newPath, _, _ := strings.Cut(raw, "\t") // "new\told" → new
				categorize(line[2], line[3], newPath, st)
			}
		case '?':
			if len(line) > 2 {
				appendFile(&st.UntrackedFiles, File{Path: line[2:], Status: "?"})
			}
		default:
			// 'u' (unmerged) / '!' (ignored) — not surfaced in the dashboard.
		}
	}
	// Detached HEAD reports "(detached)"; use the short SHA instead.
	if head == "(detached)" {
		if len(oid) > 7 {
			oid = oid[:7]
		}
		st.Branch = oid
	} else {
		st.Branch = head
	}
	return st
}

// parseHeader consumes a `# branch.*` line.
func parseHeader(line string, oid, head *string, st *Status) {
	key, value, _ := strings.Cut(strings.TrimPrefix(line, "# "), " ")
	switch key {
	case "branch.oid":
		*oid = value
	case "branch.head":
		*head = value
	case "branch.upstream":
		st.TrackingBranch = value
	case "branch.ab":
		for _, token := range strings.Fields(value) {
			if n, err := strconv.Atoi(token[1:]); err == nil {
				if token[0] == '+' {
					st.Ahead = n
				} else if token[0] == '-' {
					st.Behind = n
				}
			}
		}
	}
}

// lastField returns everything after the fixed-count leading fields of a
// changed-entry line — capping the split count keeps spaces in the path intact.
func lastField(line string, fieldCount int) string {
	pieces := strings.SplitN(line, " ", fieldCount)
	return pieces[len(pieces)-1]
}

// categorize maps a porcelain XY code (X = index/staged, Y = worktree) into the
// file buckets.
func categorize(x, y byte, path string, st *Status) {
	switch x {
	case 'M':
		appendFile(&st.StagedFiles, File{Path: path, Status: "M"})
	case 'A':
		appendFile(&st.StagedFiles, File{Path: path, Status: "A"})
	case 'D':
		appendFile(&st.StagedFiles, File{Path: path, Status: "D"})
	case 'R', 'C':
		appendFile(&st.StagedFiles, File{Path: path, Status: "R"})
	}
	switch y {
	case 'M':
		appendFile(&st.ModifiedFiles, File{Path: path, Status: "M"})
	case 'D':
		appendFile(&st.DeletedFiles, File{Path: path, Status: "D"})
	}
}

func appendFile(bucket *[]File, f File) {
	if len(*bucket) < maxFilesPerBucket {
		*bucket = append(*bucket, f)
	}
}

// run executes git in dir with a hard timeout, returning stdout and the exit
// code. code -1 with an error means git couldn't run at all (transient).
func run(ctx context.Context, dir string, args ...string) (string, int, error) {
	cctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(cctx, "git", args...)
	cmd.Dir = dir
	var stdout bytes.Buffer
	cmd.Stdout = &stdout
	if err := cmd.Run(); err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			return stdout.String(), ee.ExitCode(), nil
		}
		return "", -1, err
	}
	return stdout.String(), 0, nil
}
