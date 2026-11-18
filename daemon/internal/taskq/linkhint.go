package taskq

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// linkHint names the untracked links in left (Uncommitted's list) and how to
// replace them, "" when there are none. A worker that links the main
// checkout's .venv into its worktree to run the checks leaves a file git
// does not ignore: a pattern like ".venv/" matches only a real folder. The
// worker then deletes the link to merge, and the checks fail without it
// (task #171, 2026-11-18). A real folder made with uv is ignored, and uv
// hard-links its files from its cache, so it costs a few megabytes.
func linkHint(worktree, left string) string {
	var links []string
	for _, entry := range strings.Split(left, ", ") {
		path, ok := strings.CutPrefix(entry, "?? ")
		if !ok {
			continue
		}
		if fi, err := os.Lstat(filepath.Join(worktree, path)); err == nil && fi.Mode()&os.ModeSymlink != 0 {
			links = append(links, path)
		}
	}
	if len(links) == 0 {
		return ""
	}
	return fmt.Sprintf("; %s is a link, which git does not ignore the way it ignores a real folder: "+
		"do not just delete it if the checks need it, replace it with a real one "+
		"(for Python: uv venv, then uv pip install the repo's requirements)", strings.Join(links, " and "))
}
