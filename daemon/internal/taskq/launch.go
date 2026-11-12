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
func briefText(t store.Task, p store.Part, l layout, trunk string, team []string, servers string) string {
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

%s
`, t.ID, t.Title, filepath.Base(p.RepoPath), l.worktree, l.branch, trunk, l.busName,
		teamLine(team), BusName("<repo>", t.ID), servers) + briefDecisions + briefDone + "\n## Your plan\n\n" + p.Plan + "\n"
}

// briefDecisions is the brief's rule for a choice the plan did not settle,
// or settled with "stop and ask". In the first forty tasks workers stopped
// eleven times for a person; each time they had a recommendation and the
// person took it, after hours. So the worker decides and says so, and
// stops only for what cannot be undone or guessed. It comes before the
// plan and names the plan's own stop rules, because the plan is what told
// those workers to stop.
const briefDecisions = `## Decisions are yours

A question here costs hours: nobody is there to answer, and the tasks
queued behind this one wait with it. It is almost always a question you
can answer yourself. You have a recommendation, and the person would take
it.

So when you reach a choice and you have a recommendation:
take it, and go on. This holds where your plan says to stop, to ask, or to
report stuck (a STOP condition, a "hard rule", a file outside its list, a
file it says to leave alone) and the fix you recommend needs exactly that:
the plan was written before anyone saw what you see now. Keep the change
as small as the fix needs. Then tell the person: start your report's note
with "DECISIONS I MADE" and one line for each, saying what the plan said,
what you did, and why. All of it is commits on this branch, which a person
can read and undo. The same goes when a review of your commit tells you to
stop and propose a redesign: choose the one you would propose, build it,
and list it.

Report stuck only when:
- the choice cannot be undone from this branch: spending money, deleting
  or changing real data, applying a migration, sending anything to people,
  or changing anything outside your worktree;
- you lack access you cannot get yourself (a secret, a login, a service);
- you have no recommendation: you would be guessing at what the person
  wants;
- the checks stay red after you tried to fix them.

The rules of this brief itself are not yours to waive: no push, nothing
under .claude/, the shared database, the person's servers.
Do not ask with a question card (AskUserQuestion): nobody sees it. Decide,
or report stuck.

`

// briefDone is the brief's rules for the end of the work. Their order is
// the point: everything first, the report last, nothing after. A worker
// that reported and then ran the checks again (its Stop hook asking for a
// receipt) failed its own merge, and one that ended its turn to wait for a
// review that found nothing sat for five hours.
const briefDone = `## When you are done

- Finish everything before you report. Commit, run the repo's checks, and
  if the repo wants a signed receipt for a green run (its Stop hook says
  so), sign it before you report, not after.
- If something reviews your commits in the background, wait for it
  yourself: look whether it is still running, and deal with what it finds.
  A clean review sends you nothing, so never end your turn to wait for
  one. End your turn to wait only for what will surely wake you (a
  background command you started does, when it exits).
- Then report, as the last thing you do: call the tasks tool with action
  "report", status "done", and a note. Your decisions first, if you made
  any, then a few lines on what you built and how you checked it.
- After the report, run nothing in this folder: no tests, no checks, no
  commits. ccmux runs the repo's checks here once your turn has ended,
  and a second run beside them makes both fail.
- If you cannot go on, for one of the reasons above, commit what you have,
  call the tasks tool with action "report", status "stuck" and a note
  saying exactly what you need, then stop.
`

// teamLine names the other parts' Claudes, or says there are none yet.
func teamLine(team []string) string {
	if len(team) == 0 {
		return "No other repo has a part in this feature yet."
	}
	return "The other parts are built by: " + strings.Join(team, ", ") +
		" (all starting now; list_peers shows who is up)."
}

func writeBrief(t store.Task, p store.Part, l layout, trunk string, team []string, servers string) error {
	if err := os.MkdirAll(l.dir, 0o755); err != nil {
		return err
	}
	return os.WriteFile(l.brief, []byte(briefText(t, p, l, trunk, team, servers)), 0o644)
}

// launchCommand starts the worker's Claude: on the peers bus under its own
// name (so the repo's own session keeps the repo's name), with the model
// and effort its part was filed with, in auto mode because nobody is there
// to answer a permission prompt, allowed to read its brief beside the
// worktree, and told to begin with it. Every flag comes before the "--":
// what follows it is the prompt.
func launchCommand(l layout, run Run) string {
	prompt := fmt.Sprintf("Read %s and carry out the task it describes.", l.brief)
	return "env -u CLAUDE_PEERS_PROJECT CLAUDE_PEERS_NAME=" + shellQuote(l.busName) +
		" claude --dangerously-load-development-channels server:claude-peers" +
		" --model " + shellQuote(run.Model) + " --effort " + shellQuote(run.Effort) +
		" --permission-mode auto --add-dir " + shellQuote(l.dir) + " -- " + shellQuote(prompt)
}

// shellQuote wraps s for POSIX shells.
func shellQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }
