package api

import (
	"bytes"
	"context"
	"errors"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"

	"ccmux.dev/ccmuxd/internal/gitstatus"
	"ccmux.dev/ccmuxd/internal/store"
)

// The thread page's Git tab: the commits in a session's repo that have
// not shipped yet, in groups by how far they still have to go. A repo with
// a dev branch ships dev to main; a repo with only main ships by pushing
// it, so "not shipped" there is what came since the last v* tag.

const (
	commitsShown = 100 // per group; the rest is a count
	commitsWait  = 10 * time.Second
)

// The refs a repo's groups are read from, always by their full names: a
// branch someone called "origin/dev" is not the remote's dev.
const (
	refDev       = "refs/heads/dev"
	refOriginDev = "refs/remotes/origin/dev"
)

func refHead(name string) string   { return "refs/heads/" + name }
func refOrigin(name string) string { return "refs/remotes/origin/" + name }

// trunks are the names a repo's release branch goes by, the first found
// counts.
var trunks = []string{"main", "master"}

type commitRow struct {
	SHA     string `json:"sha"`
	Short   string `json:"short"`
	Subject string `json:"subject"`
	Body    string `json:"body"` // the message under the subject, trimmed
	Author  string `json:"author"`
	Time    int64  `json:"time"` // unix ms, as the page's other times
}

type commitGroup struct {
	Key     string      `json:"key"`
	Title   string      `json:"title"`
	Commits []commitRow `json:"commits"`
	More    int         `json:"more"` // commits in the group past the ones listed
}

type commitsView struct {
	Repo   bool          `json:"repo"`
	Branch string        `json:"branch"`
	Groups []commitGroup `json:"groups"`
	// Git is the folder's own status (branch, changed files, ahead and
	// behind its upstream and its trunk), the one line over the groups.
	// It is the session's folder, a worktree included, not the repo's main
	// one. Absent when it could not be read: the groups still stand.
	Git *gitstatus.Status `json:"git,omitempty"`
}

// commitRange is one group before git is asked for it: the commits
// reachable from to and not from from.
type commitRange struct {
	key, title, from, to string
}

// sessionCommits: GET /v1/sessions/{id}/commits.
func (s *Server) sessionCommits(w http.ResponseWriter, r *http.Request) {
	sess := s.sessionFolder(w, r, "commits")
	if sess == nil {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), commitsWait)
	defer cancel()
	view, err := listCommits(ctx, sess.Cwd)
	if errors.Is(err, errNotRepo) {
		writeJSON(w, http.StatusOK, commitsView{Groups: []commitGroup{}})
		return
	}
	if err != nil {
		log.Printf("sessions: commits of %s in %s: %v", sess.ID, sess.Cwd, err)
		writeError(w, http.StatusBadGateway, "the commits could not be read: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, view)
}

// sessionFolder is the session the route names, or nil when it answered
// already (runners off, no such session, a row that could not be read).
func (s *Server) sessionFolder(w http.ResponseWriter, r *http.Request, what string) *store.RunnerSession {
	if !s.runnersReady(w) {
		return nil
	}
	sess, err := s.runnerRows.RunnerSession(r.PathValue("id"))
	if err != nil {
		log.Printf("sessions: %s of %s: row not read: %v", what, r.PathValue("id"), err)
		writeError(w, http.StatusInternalServerError, "the session could not be read: "+err.Error())
		return nil
	}
	if sess == nil {
		writeRunnerError(w, store.ErrNoSession)
	}
	return sess
}

// listCommits reads dir's groups. A folder git does not know is errNotRepo.
func listCommits(ctx context.Context, dir string) (commitsView, error) {
	if err := workTree(ctx, dir); err != nil {
		return commitsView{}, err
	}
	has, err := knownRefs(ctx, dir)
	if err != nil {
		return commitsView{}, err
	}
	out, err := git(ctx, dir, "branch", "--show-current")
	if err != nil {
		return commitsView{}, err
	}
	head := strings.TrimSpace(string(out)) // "" when detached
	trunk := trunkOf(has)
	tag, err := lastRelease(ctx, dir, has, trunk)
	if err != nil {
		return commitsView{}, err
	}
	view := commitsView{Repo: true, Branch: head, Groups: []commitGroup{}, Git: folderStatus(ctx, dir)}
	for _, rg := range pickRanges(has, head, trunk, tag) {
		g, err := readGroup(ctx, dir, rg)
		if err != nil {
			return commitsView{}, err
		}
		if len(g.Commits) > 0 {
			view.Groups = append(view.Groups, g)
		}
	}
	return view, nil
}

// workTree says dir is inside a git work tree, errNotRepo when it is not.
func workTree(ctx context.Context, dir string) error {
	inside, err := git(ctx, dir, "rev-parse", "--is-inside-work-tree")
	if err != nil {
		return err
	}
	if strings.TrimSpace(string(inside)) != "true" {
		return errNotRepo // inside a .git folder: no work tree
	}
	return nil
}

// knownRefs is which of the refs the groups are read from exist. for-each-ref
// answers nothing (and no failure) for a ref that is not there, and its
// patterns also match below a slash (refs/heads/dev/x), so only exact
// names count.
func knownRefs(ctx context.Context, dir string) (map[string]bool, error) {
	want := []string{refDev, refOriginDev}
	for _, t := range trunks {
		want = append(want, refHead(t), refOrigin(t))
	}
	out, err := git(ctx, dir, append([]string{"for-each-ref", "--format=%(refname)"}, want...)...)
	if err != nil {
		return nil, err
	}
	return exactRefs(out, want), nil
}

func exactRefs(out []byte, want []string) map[string]bool {
	has := map[string]bool{}
	for _, line := range strings.Split(string(out), "\n") {
		for _, w := range want {
			if line == w {
				has[w] = true
			}
		}
	}
	return has
}

// trunkOf is the repo's release branch name, "" for none.
func trunkOf(has map[string]bool) string {
	for _, t := range trunks {
		if has[refHead(t)] || has[refOrigin(t)] {
			return t
		}
	}
	return ""
}

// lastRelease is the newest v* tag on the remote's trunk, "" for none. Only
// a repo with no dev branch needs it.
func lastRelease(ctx context.Context, dir string, has map[string]bool, trunk string) (string, error) {
	if has[refDev] || has[refOriginDev] || trunk == "" || !has[refOrigin(trunk)] {
		return "", nil
	}
	out, err := git(ctx, dir, "for-each-ref", "--merged="+refOrigin(trunk), "--sort=-version:refname", "--count=1", "--format=%(refname)", "refs/tags/v*")
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

// pickRanges is the groups a repo has, in the order the tab shows them.
// Every range has a base: a group that would be the whole history is left
// out.
func pickRanges(has map[string]bool, head, trunk, tag string) []commitRange {
	if has[refDev] || has[refOriginDev] {
		return devRanges(has, head, trunk)
	}
	return trunkRanges(has, head, trunk, tag)
}

// firstRef is the first of refs that exists, "" for none.
func firstRef(has map[string]bool, refs ...string) string {
	for _, r := range refs {
		if has[r] {
			return r
		}
	}
	return ""
}

// onBranch is the group of a branch that is neither dev nor the trunk (a
// task's or a thread's worktree): what it holds past base.
func onBranch(head, base, trunk string) []commitRange {
	if head == "" || head == "dev" || head == trunk || base == "" {
		return nil
	}
	return []commitRange{{key: "branch", title: "On this branch (" + head + ")", from: base, to: "HEAD"}}
}

func devRanges(has map[string]bool, head, trunk string) []commitRange {
	dev := firstRef(has, refDev, refOriginDev)
	base := ""
	if trunk != "" {
		base = firstRef(has, refOrigin(trunk), refHead(trunk))
	}
	out := onBranch(head, dev, trunk)
	switch {
	case has[refDev] && has[refOriginDev]:
		out = append(out, commitRange{key: "unpushed", title: "Not pushed", from: refOriginDev, to: refDev})
	case has[refDev] && base != "":
		out = append(out, commitRange{key: "unpushed", title: "Not pushed", from: base, to: refDev})
	}
	if has[refOriginDev] && base != "" {
		out = append(out, commitRange{key: "pushed", title: "Pushed, not shipped", from: base, to: refOriginDev})
	}
	return out
}

func trunkRanges(has map[string]bool, head, trunk, tag string) []commitRange {
	if trunk == "" {
		return nil
	}
	local, remote := refHead(trunk), refOrigin(trunk)
	out := onBranch(head, firstRef(has, local, remote), trunk)
	if has[local] && has[remote] {
		out = append(out, commitRange{key: "unpushed", title: "Not pushed", from: remote, to: local})
	}
	if tag != "" && has[remote] {
		out = append(out, commitRange{key: "release", title: "Since the last release", from: tag, to: remote})
	}
	return out
}

// readGroup lists one range, the newest first, at most commitsShown of it.
func readGroup(ctx context.Context, dir string, rg commitRange) (commitGroup, error) {
	span := rg.from + ".." + rg.to
	out, err := git(ctx, dir, "rev-list", "--count", span, "--")
	if err != nil {
		return commitGroup{}, err
	}
	total, err := strconv.Atoi(strings.TrimSpace(string(out)))
	if err != nil {
		return commitGroup{}, err
	}
	g := commitGroup{Key: rg.key, Title: rg.title, Commits: []commitRow{}}
	if total == 0 {
		return g, nil
	}
	out, err = git(ctx, dir, "log", "--no-color", "--format=%H%x00%h%x00%s%x00%an%x00%at%x00%b%x1e", "--max-count="+strconv.Itoa(commitsShown), span, "--")
	if err != nil {
		return commitGroup{}, err
	}
	g.Commits = parseLog(out)
	g.More = max(0, total-len(g.Commits))
	return g, nil
}

// parseLog reads git log's records of sha, short sha, subject, author, time
// and body, split by NULs and ended by a record separator (a body has
// lines of its own). A record it cannot read is left out.
func parseLog(out []byte) []commitRow {
	rows := []commitRow{}
	for _, rec := range bytes.Split(out, []byte{0x1e}) {
		f := strings.Split(strings.TrimLeft(string(rec), "\n"), "\x00")
		if len(f) != 6 {
			continue
		}
		at, _ := strconv.ParseInt(f[4], 10, 64)
		rows = append(rows, commitRow{SHA: f[0], Short: f[1], Subject: f[2], Body: strings.TrimSpace(f[5]), Author: f[3], Time: at * 1000})
	}
	return rows
}

// folderStatus is dir's git status for the line over the groups, or nil
// (logged) when it could not be read.
func folderStatus(ctx context.Context, dir string) *gitstatus.Status {
	st, err := gitstatus.Full(ctx, dir, gitstatus.DetectDefaultBranch(ctx, dir))
	if err != nil {
		log.Printf("sessions: git status of %s not read: %v", dir, err)
		return nil
	}
	return st
}
