package memtidy

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"time"

	"ccmux.dev/ccmuxd/internal/store"
)

// repo is one repo whose memory is tidied, and every folder a session of
// it worked in: its own and its builders' and helpers' worktrees. Claude
// Code keeps each folder's conversations apart, but files a worktree's
// memory under the repo it belongs to.
type repo struct {
	Path string
	Cwds []string
}

// reposOf groups the session rows by the repo they worked for. A role
// agent is left out: its memory lives in its own home, not in a repo's.
func reposOf(rows []store.RunnerSession) []repo {
	cwds := map[string]map[string]bool{}
	for _, r := range rows {
		path := repoOf(r)
		if path == "" {
			continue
		}
		if cwds[path] == nil {
			cwds[path] = map[string]bool{}
		}
		for _, c := range []string{path, r.Cwd, r.Worktree} {
			if c != "" {
				cwds[path][c] = true
			}
		}
	}
	out := make([]repo, 0, len(cwds))
	for path, set := range cwds {
		out = append(out, repo{Path: path, Cwds: sortedKeys(set)})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out
}

// repoOf is the repo a session worked for: its thread's repo, or the
// folder it runs in. "" for a role agent.
func repoOf(r store.RunnerSession) string {
	if r.Kind == store.SessionAgent {
		return ""
	}
	if r.Repo != "" {
		return filepath.Clean(r.Repo)
	}
	if r.Cwd == "" {
		return ""
	}
	return filepath.Clean(r.Cwd)
}

func sortedKeys(set map[string]bool) []string {
	out := make([]string, 0, len(set))
	for k := range set {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

var notProjectChar = regexp.MustCompile(`[^A-Za-z0-9]`)

// projectDir is the folder Claude Code keeps a cwd's conversations (and,
// for a repo's own folder, its memory) in: <projects>/<cwd with every
// other character a dash>.
func projectDir(projects, cwd string) string {
	return filepath.Join(projects, notProjectChar.ReplaceAllString(cwd, "-"))
}

// conversation is one transcript file and when it was last written.
type conversation struct {
	Path  string
	MTime time.Time
}

// conversationsOf lists the transcripts of a repo's folders written after
// since and up to until, oldest first. A folder with none is skipped.
func conversationsOf(projects string, r repo, since, until time.Time) []conversation {
	var out []conversation
	for _, cwd := range r.Cwds {
		files, _ := filepath.Glob(filepath.Join(projectDir(projects, cwd), "*.jsonl"))
		for _, f := range files {
			fi, err := os.Stat(f)
			if err != nil || !fi.Mode().IsRegular() {
				continue
			}
			if m := fi.ModTime(); m.After(since) && !m.After(until) {
				out = append(out, conversation{Path: f, MTime: m})
			}
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].MTime.Before(out[j].MTime) })
	return out
}

// isRepo says whether a folder is still there and is a repo's own folder:
// its .git is a folder. A worktree's is a file, and its memory is the
// repo's anyway.
func isRepo(path string) bool {
	fi, err := os.Stat(filepath.Join(path, ".git"))
	return err == nil && fi.IsDir()
}
