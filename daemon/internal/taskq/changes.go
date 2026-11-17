package taskq

import (
	"strconv"
	"strings"
)

// BranchChanges is what a worktree's branch holds that its trunk does not:
// how many commits, the files they change with lines added and removed, and
// how many files in the worktree are not committed yet. It is what a page
// shows of work before it lands; the merge reads none of it.
type BranchChanges struct {
	Commits     int          `json:"commits"`
	Uncommitted int          `json:"uncommitted"`
	Files       []FileChange `json:"files,omitempty"`
}

// FileChange is one changed file. A binary file has no line counts.
type FileChange struct {
	Path    string `json:"path"`
	Added   int    `json:"added"`
	Removed int    `json:"removed"`
}

// ReadBranchChanges reads a worktree's changes against trunk: committed
// ones since the branch left it, and the count of uncommitted files (the
// copied local settings never count, as in a merge).
func ReadBranchChanges(worktree, trunk string) (BranchChanges, error) {
	var out BranchChanges
	count, err := runGit(worktree, "rev-list", "--count", trunk+"..HEAD")
	if err != nil {
		return out, err
	}
	out.Commits, _ = strconv.Atoi(count)
	stat, err := runGit(worktree, "diff", "--numstat", "--no-renames", trunk+"...HEAD")
	if err != nil {
		return out, err
	}
	out.Files = numstat(stat)
	left, err := ExecGit{}.Uncommitted(worktree)
	if err != nil {
		return out, err
	}
	if left != "" {
		out.Uncommitted = len(strings.Split(left, ", "))
	}
	return out, nil
}

// numstat reads `git diff --numstat` lines: added, removed, path.
func numstat(text string) []FileChange {
	var out []FileChange
	for _, line := range strings.Split(text, "\n") {
		f := strings.SplitN(line, "\t", 3)
		if len(f) != 3 {
			continue
		}
		added, _ := strconv.Atoi(f[0]) // "-" for a binary file: 0
		removed, _ := strconv.Atoi(f[1])
		out = append(out, FileChange{Path: f[2], Added: added, Removed: removed})
	}
	return out
}
