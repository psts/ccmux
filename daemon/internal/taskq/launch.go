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

// layout is where one task is built: its worktree, branch, brief and the
// name its Claude carries on the peers bus.
type layout struct {
	dir, worktree, brief, branch, busName string
}

// layoutOf places task t under root. The repo's folder name is joined by a
// short hash of its path, so two repos that share a name never share a folder.
func layoutOf(root string, t store.Task) layout {
	repo := filepath.Base(t.RepoPath)
	sum := sha1.Sum([]byte(t.RepoPath))
	dir := filepath.Join(root, repo+"-"+hex.EncodeToString(sum[:3]))
	name := fmt.Sprintf("task-%d", t.ID)
	branch := t.Branch // a task run before keeps its branch, whatever its title says now
	if branch == "" {
		branch = fmt.Sprintf("task/%d-%s", t.ID, slug(t.Title))
	}
	return layout{
		dir:      dir,
		worktree: filepath.Join(dir, name),
		brief:    filepath.Join(dir, name+".md"),
		branch:   branch,
		busName:  fmt.Sprintf("%s-task-%d", repo, t.ID),
	}
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
// unattended, then the plan its repo's session filed.
func briefText(t store.Task, l layout, trunk string) string {
	return fmt.Sprintf(`# ccmux task #%d: %s

You are building this task on your own, in your own git worktree. Nobody is
watching; a person reads the result later.

- Repo: %s
- Your working folder (a git worktree): %s
- Branch: %s, made from %s. Commit your work on this branch.
- Do not push, merge, rebase or switch branches. ccmux merges your branch after you.
- Follow the repo's CLAUDE.md, run its checks, and leave them green.
- When the work is done and committed, call the tasks tool with action "report",
  status "done", and a note: a few lines on what you built and how you checked it.
- If you cannot go on (a choice only a person can make, missing access, checks you
  cannot fix), commit what you have, call the tasks tool with action "report",
  status "stuck" and a note saying what you need, then stop.
- Other repos' sessions are on the peers bus if the plan needs them. Your name there
  is %s.

## The plan

%s
`, t.ID, t.Title, t.RepoPath, l.worktree, l.branch, trunk, l.busName, t.Plan)
}

func writeBrief(t store.Task, l layout, trunk string) error {
	if err := os.MkdirAll(l.dir, 0o755); err != nil {
		return err
	}
	return os.WriteFile(l.brief, []byte(briefText(t, l, trunk)), 0o644)
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
