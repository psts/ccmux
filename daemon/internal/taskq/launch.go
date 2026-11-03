package taskq

import (
	"crypto/sha1"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"ccmux.dev/ccmuxd/internal/store"
)

// layout is where one part of a task is built: its worktree, branch, brief
// and the name its Claude carries on the peers bus.
type layout struct {
	dir, worktree, brief, branch, busName string
}

// layoutOf places part p of task t under root. The repo's folder name is
// joined by a short hash of its path, so two repos that share a name never
// share a folder. Every part of a task gets the same branch name, each in
// its own repo, and a bus name its teammates can work out: <repo>-task-<id>.
func layoutOf(root string, t store.Task, p store.Part) layout {
	sum := sha1.Sum([]byte(p.RepoPath))
	dir := filepath.Join(root, filepath.Base(p.RepoPath)+"-"+hex.EncodeToString(sum[:3]))
	name := fmt.Sprintf("task-%d", t.ID)
	branch := p.Branch // a part run before keeps its branch, whatever the title says now
	if branch == "" {
		branch = fmt.Sprintf("task/%d-%s", t.ID, slug(t.Title))
	}
	return layout{
		dir:      dir,
		worktree: filepath.Join(dir, name),
		brief:    filepath.Join(dir, name+".md"),
		branch:   branch,
		busName:  BusName(p.RepoPath, t.ID),
	}
}

// BusName is the peers-bus name of the Claude building a task's part in repo.
func BusName(repo string, taskID int64) string {
	return fmt.Sprintf("%s-task-%d", filepath.Base(repo), taskID)
}

// slug turns a title into a branch-safe word: lower case, letters, digits
// and single dashes, at most 40 characters.
func slug(title string) string {
	var b strings.Builder
	dash := false
	for _, r := range strings.ToLower(title) {
		ok := (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9')
		switch {
		case ok:
			b.WriteRune(r)
			dash = false
		case !dash && b.Len() > 0:
			b.WriteByte('-')
			dash = true
		}
		if b.Len() >= 40 {
			break
		}
	}
	s := strings.Trim(b.String(), "-")
	if s == "" {
		return "task"
	}
	return s
}

// briefText is what the worker reads first: the rules of working
// unattended, who else is building this feature, then its own plan.
func briefText(t store.Task, p store.Part, l layout, trunk string, team []string) string {
	return fmt.Sprintf(`# ccmux task #%d: %s

You are building one part of this feature, the part in %s, on your own and
in your own git worktree. Nobody is watching; a person reads the result later.

- Your working folder (a git worktree): %s
- Branch: %s, made from %s. Commit your work on this branch.
- Do not push, merge, rebase or switch branches. ccmux merges every part of
  this feature together once all of them are done.
- Follow the repo's CLAUDE.md, run its checks, and leave them green. Do not
  change anything under .claude/ (the repo's gates live there): a part that
  does is not merged automatically, it waits for a person.

## Your team

Your name on the peers bus is %s. %s
- Start by telling your teammates you have started and what your part covers.
- Talk to them directly when your part needs something from theirs (an API
  endpoint, a field, a setting) or changes something they depend on. Ask them to
  fold it into their part; do not build it in their repo yourself.
- If the feature needs a repo that has no part yet, call the tasks tool with
  action "add_part", that repo's folder name, and a plan for it. A Claude starts
  on it at once, as %s.

## When you are done

- When your part is done and committed, call the tasks tool with action "report",
  status "done", and a note: a few lines on what you built and how you checked it.
- If you cannot go on (a choice only a person can make, missing access, checks you
  cannot fix), commit what you have, call the tasks tool with action "report",
  status "stuck" and a note saying what you need, then stop.

## Your plan

%s
`, t.ID, t.Title, filepath.Base(p.RepoPath), l.worktree, l.branch, trunk, l.busName,
		teamLine(team), BusName("<repo>", t.ID), p.Plan)
}

// teamLine names the other parts' Claudes, or says there are none yet.
func teamLine(team []string) string {
	if len(team) == 0 {
		return "No other repo has a part in this feature yet."
	}
	return "The other parts are built by: " + strings.Join(team, ", ") +
		" (all starting now; list_peers shows who is up)."
}

func writeBrief(t store.Task, p store.Part, l layout, trunk string, team []string) error {
	if err := os.MkdirAll(l.dir, 0o755); err != nil {
		return err
	}
	return os.WriteFile(l.brief, []byte(briefText(t, p, l, trunk, team)), 0o644)
}

// launchCommand starts the worker's Claude: on the peers bus under its own
// name (so the repo's own session keeps the repo's name), in auto mode
// because nobody is there to answer a permission prompt, allowed to read
// its brief beside the worktree, and told to begin with it.
func launchCommand(l layout) string {
	prompt := fmt.Sprintf("Read %s and carry out the task it describes.", l.brief)
	return "env -u CLAUDE_PEERS_PROJECT CLAUDE_PEERS_NAME=" + shellQuote(l.busName) +
		" claude --dangerously-load-development-channels server:claude-peers" +
		" --permission-mode auto --add-dir " + shellQuote(l.dir) + " -- " + shellQuote(prompt)
}

// shellQuote wraps s for POSIX shells.
func shellQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }
