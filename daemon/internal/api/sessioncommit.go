package api

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
)

// One commit of the Commits tab: its message, and per file its lines added
// and removed and its diff, drawn by the page's View.diff.

const (
	commitLines     = 3000    // diff lines shown for one commit, all files together
	commitLineChars = 1000    // a longer line (minified code) is cut
	commitPatchRead = 4 << 20 // bytes of patch read at most
)

var shaRe = regexp.MustCompile(`^[0-9a-f]{7,64}$`)

type diffHunk struct {
	OldStart int      `json:"oldStart"`
	NewStart int      `json:"newStart"`
	Lines    []string `json:"lines"`
}

type fileDiff struct {
	Hunks     []diffHunk `json:"hunks"`
	Truncated bool       `json:"truncated"`
}

type commitFile struct {
	Path    string   `json:"path"`
	From    string   `json:"from,omitempty"` // a renamed file's old path
	Added   int      `json:"added"`
	Removed int      `json:"removed"`
	Binary  bool     `json:"binary,omitempty"`
	Diff    fileDiff `json:"diff"`
}

type commitDetail struct {
	SHA     string       `json:"sha"`
	Subject string       `json:"subject"`
	Body    string       `json:"body"`
	Author  string       `json:"author"`
	Time    int64        `json:"time"` // unix ms
	Files   []commitFile `json:"files"`
}

var errNoCommit = errors.New("no such commit in this repo")

// sessionCommit: GET /v1/sessions/{id}/commits/{sha}.
func (s *Server) sessionCommit(w http.ResponseWriter, r *http.Request) {
	sha := strings.ToLower(r.PathValue("sha"))
	if !shaRe.MatchString(sha) {
		writeError(w, http.StatusBadRequest, "a commit is named by 7 to 64 hex digits")
		return
	}
	sess := s.sessionFolder(w, r, "commit")
	if sess == nil {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), commitsWait)
	defer cancel()
	d, err := readCommit(ctx, sess.Cwd, sha)
	switch {
	case errors.Is(err, errNotRepo):
		writeError(w, http.StatusNotFound, "this session's folder is not a git repo")
	case errors.Is(err, errNoCommit):
		writeError(w, http.StatusNotFound, errNoCommit.Error())
	case err != nil:
		log.Printf("sessions: commit %s of %s in %s: %v", sha, sess.ID, sess.Cwd, err)
		writeError(w, http.StatusBadGateway, "the commit could not be read: "+err.Error())
	default:
		writeJSON(w, http.StatusOK, d)
	}
}

// showArgs are what every git show of one commit runs with: a merge is
// shown against its first parent (not git's combined diff, which has no
// numstat), renames are found, and nothing from the repo's config turns
// the diff into another program's output.
var showArgs = []string{"show", "--no-color", "--no-ext-diff", "--no-textconv", "--diff-merges=first-parent", "-M"}

// readCommit reads sha (checked as hex by the caller) in dir.
func readCommit(ctx context.Context, dir, sha string) (commitDetail, error) {
	if err := workTree(ctx, dir); err != nil {
		return commitDetail{}, err
	}
	if err := isCommit(ctx, dir, sha); err != nil {
		return commitDetail{}, err
	}
	head, err := git(ctx, dir, append(showArgs, "-s", "--format=%H%x00%an%x00%at%x00%s%x00%b", sha, "--")...)
	if err != nil {
		return commitDetail{}, err
	}
	d, err := parseCommitHead(head)
	if err != nil {
		return commitDetail{}, err
	}
	stat, err := git(ctx, dir, append(showArgs, "--format=", "--numstat", "-z", sha, "--")...)
	if err != nil {
		return commitDetail{}, err
	}
	patch, cut, err := gitCapped(ctx, dir, commitPatchRead, append(showArgs, "--format=", "--patch", sha, "--")...)
	if err != nil {
		return commitDetail{}, err
	}
	d.Files = joinDiffs(parseNumstat(stat), parsePatch(patch, cut))
	return d, nil
}

// isCommit says sha names a commit in dir, errNoCommit when it does not
// (unknown, another kind of object, or a short sha two objects share).
func isCommit(ctx context.Context, dir, sha string) error {
	kind, err := git(ctx, dir, "cat-file", "-t", sha)
	if err != nil && ctx.Err() == nil {
		return errNoCommit
	}
	if err != nil {
		return err
	}
	if strings.TrimSpace(string(kind)) != "commit" {
		return errNoCommit
	}
	return nil
}

// parseCommitHead reads the NUL-split line of sha, author, time, subject
// and body.
func parseCommitHead(out []byte) (commitDetail, error) {
	f := strings.SplitN(string(out), "\x00", 5)
	if len(f) != 5 {
		return commitDetail{}, fmt.Errorf("git show: %d fields, want 5", len(f))
	}
	at, _ := strconv.ParseInt(f[2], 10, 64)
	return commitDetail{SHA: f[0], Author: f[1], Time: at * 1000, Subject: f[3], Body: strings.TrimSpace(f[4]), Files: []commitFile{}}, nil
}

// parseNumstat reads git's --numstat -z: per file "added TAB removed TAB
// path NUL", or for a rename "added TAB removed TAB NUL old NUL new NUL".
// A binary file has "-" for both counts.
func parseNumstat(out []byte) []commitFile {
	tok := strings.Split(string(out), "\x00")
	files := []commitFile{}
	for i := 0; i < len(tok); i++ {
		f := strings.SplitN(tok[i], "\t", 3)
		if len(f) != 3 {
			continue
		}
		cf := commitFile{Path: f[2], Binary: f[0] == "-"}
		cf.Added, _ = strconv.Atoi(f[0])
		cf.Removed, _ = strconv.Atoi(f[1])
		if f[2] == "" && i+2 < len(tok) {
			cf.From, cf.Path = tok[i+1], tok[i+2]
			i += 2
		}
		files = append(files, cf)
	}
	return files
}

// patchReader cuts one commit's patch into files and hunks, keeping at most
// commitLines lines in all.
type patchReader struct {
	files []fileDiff
	kept  int
	hunk  bool // in a hunk: lines are the change, not a file's header
}

func (p *patchReader) line(l string) {
	switch {
	case strings.HasPrefix(l, "diff --git "):
		p.files = append(p.files, fileDiff{Hunks: []diffHunk{}})
		p.hunk = false
	case len(p.files) == 0:
	case strings.HasPrefix(l, "@@ "):
		p.startHunk(l)
	case p.hunk && l != "" && strings.ContainsRune("+- \\", rune(l[0])):
		p.keep(l)
	}
}

func (p *patchReader) cur() *fileDiff { return &p.files[len(p.files)-1] }

func (p *patchReader) startHunk(l string) {
	p.hunk = true
	if p.kept >= commitLines {
		p.cur().Truncated = true
		return
	}
	old, nw := hunkStarts(l)
	p.cur().Hunks = append(p.cur().Hunks, diffHunk{OldStart: old, NewStart: nw, Lines: []string{}})
}

func (p *patchReader) keep(l string) {
	f := p.cur()
	if p.kept >= commitLines || len(f.Hunks) == 0 {
		f.Truncated = true
		return
	}
	if len(l) > commitLineChars {
		l = l[:commitLineChars] + "…"
	}
	h := &f.Hunks[len(f.Hunks)-1]
	h.Lines = append(h.Lines, l)
	p.kept++
}

var hunkRe = regexp.MustCompile(`^@@ -(\d+)(?:,\d+)? \+(\d+)(?:,\d+)? @@`)

// hunkStarts is a hunk's first line in the old and the new file.
func hunkStarts(l string) (int, int) {
	m := hunkRe.FindStringSubmatch(l)
	if m == nil {
		return 0, 0
	}
	old, _ := strconv.Atoi(m[1])
	nw, _ := strconv.Atoi(m[2])
	return old, nw
}

// parsePatch is each file's diff, in git's order. cut says the patch was
// read only in part: its last file is marked so.
func parsePatch(out []byte, cut bool) []fileDiff {
	p := &patchReader{}
	for _, l := range strings.Split(string(out), "\n") {
		p.line(l)
	}
	if cut && len(p.files) > 0 {
		p.cur().Truncated = true
	}
	return p.files
}

// joinDiffs puts each file's diff on its numstat row: git lists the files
// in the same order in both. A file the patch did not reach (read in part)
// is marked as cut.
func joinDiffs(files []commitFile, diffs []fileDiff) []commitFile {
	for i := range files {
		if i < len(diffs) {
			files[i].Diff = diffs[i]
		} else {
			files[i].Diff = fileDiff{Hunks: []diffHunk{}, Truncated: !files[i].Binary}
		}
	}
	return files
}

// gitCapped is git() for a command whose output can be huge: it reads at
// most limit bytes and says whether it stopped there.
func gitCapped(ctx context.Context, dir string, limit int64, args ...string) ([]byte, bool, error) {
	cmd := exec.CommandContext(ctx, "git", append([]string{"-C", dir}, args...)...)
	cmd.Env = append(os.Environ(), "LC_ALL=C")
	var said bytes.Buffer
	cmd.Stderr = &said
	pipe, err := cmd.StdoutPipe()
	if err != nil {
		return nil, false, err
	}
	if err := cmd.Start(); err != nil {
		return nil, false, fmt.Errorf("git %s: %w", args[0], err)
	}
	out, err := io.ReadAll(io.LimitReader(pipe, limit+1))
	cut := int64(len(out)) > limit
	if cut {
		out = out[:limit]
		_ = cmd.Process.Kill() // the rest is not wanted
	}
	waitErr := cmd.Wait()
	if err == nil && !cut && waitErr != nil {
		err = waitErr
	}
	if ctx.Err() != nil {
		return nil, false, fmt.Errorf("git %s: %w", args[0], ctx.Err())
	}
	if err != nil {
		return nil, false, fmt.Errorf("git %s: %v: %s", args[0], err, strings.TrimSpace(said.String()))
	}
	return out, cut, nil
}
