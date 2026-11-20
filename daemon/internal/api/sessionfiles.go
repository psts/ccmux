package api

import (
	"bytes"
	"context"
	"errors"
	"log"
	"net/http"
	"os/exec"
	"path"
	"sort"
	"strings"
	"time"

	"ccmux.dev/ccmuxd/internal/store"
)

// The files a person can mention with "@" in a session's box on the
// thread page. The Agent SDK offers Claude Code's own file autocomplete
// only to the CLI, not to a host (no public call in SDK 0.3.311), so the
// daemon lists the files git knows in the session's folder and matches the
// typed text itself.

const (
	filesShown = 20
	filesRead  = 50000 // a repo larger than this is matched on its first files only
	filesWait  = 5 * time.Second
)

// sessionFiles: GET /v1/sessions/{id}/files?q=... answers {files: [...]},
// the best matches first. A folder that is not a git repo has none.
func (s *Server) sessionFiles(w http.ResponseWriter, r *http.Request) {
	if !s.runnersReady(w) {
		return
	}
	sess, err := s.runnerRows.RunnerSession(r.PathValue("id"))
	if err != nil {
		log.Printf("sessions: files of %s: row not read: %v", r.PathValue("id"), err)
		writeError(w, http.StatusInternalServerError, "the session could not be read: "+err.Error())
		return
	}
	if sess == nil {
		writeRunnerError(w, store.ErrNoSession)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), filesWait)
	defer cancel()
	all, err := gitFiles(ctx, sess.Cwd)
	if errors.Is(err, errNotRepo) {
		writeJSON(w, http.StatusOK, map[string]any{"files": []string{}, "note": "not a git repo: no files to mention"})
		return
	}
	if err != nil {
		log.Printf("sessions: files of %s in %s: %v", sess.ID, sess.Cwd, err)
		writeError(w, http.StatusBadGateway, "the files could not be listed: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"files": matchFiles(all, r.URL.Query().Get("q"), filesShown)})
}

// errNotRepo is a folder git does not know: it has nothing to mention,
// and that is no failure.
var errNotRepo = errors.New("not a git repository")

// gitFiles is every file git tracks or would track (not ignored) under dir.
func gitFiles(ctx context.Context, dir string) ([]string, error) {
	// Asked first, not read from ls-files' error: git words that in the
	// daemon's language.
	inside, err := exec.CommandContext(ctx, "git", "-C", dir, "rev-parse", "--is-inside-work-tree").Output()
	var exit *exec.ExitError
	if errors.As(err, &exit) || (err == nil && strings.TrimSpace(string(inside)) != "true") {
		return nil, errNotRepo
	}
	if err != nil {
		return nil, err
	}
	out, err := exec.CommandContext(ctx, "git", "-C", dir, "ls-files", "-co", "--exclude-standard", "-z").Output()
	if err != nil {
		return nil, err
	}
	var files []string
	for _, f := range bytes.Split(out, []byte{0}) {
		if len(f) > 0 && len(files) < filesRead {
			files = append(files, string(f))
		}
	}
	return files, nil
}

// matchFiles ranks paths for typed text q, case aside: a file name that
// starts with it first, then a file name that holds it, then a path that
// holds it, then a path that has its letters in order; shorter paths first
// within each. An empty q lists the shallowest files.
func matchFiles(paths []string, q string, n int) []string {
	q = strings.ToLower(strings.TrimSpace(q))
	type hit struct {
		path  string
		score int
	}
	var hits []hit
	for _, p := range paths {
		if score := matchScore(strings.ToLower(p), q); score >= 0 {
			hits = append(hits, hit{p, score})
		}
	}
	sort.SliceStable(hits, func(i, j int) bool {
		if hits[i].score != hits[j].score {
			return hits[i].score < hits[j].score
		}
		return len(hits[i].path) < len(hits[j].path)
	})
	out := []string{}
	for i := 0; i < len(hits) && i < n; i++ {
		out = append(out, hits[i].path)
	}
	return out
}

// matchScore is how well path p matches q (lower is better), -1 for no
// match.
func matchScore(p, q string) int {
	base := path.Base(p)
	switch {
	case q == "":
		return strings.Count(p, "/")
	case strings.HasPrefix(base, q):
		return 0
	case strings.Contains(base, q):
		return 1
	case strings.Contains(p, q):
		return 2
	case inOrder(p, q):
		return 3
	}
	return -1
}

// inOrder reports whether every letter of q appears in p, in order.
func inOrder(p, q string) bool {
	i := 0
	for j := 0; j < len(p) && i < len(q); j++ {
		if p[j] == q[i] {
			i++
		}
	}
	return i == len(q)
}
