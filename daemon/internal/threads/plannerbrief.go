package threads

import (
	"fmt"
	"path/filepath"
	"strings"

	"ccmux.dev/ccmuxd/internal/store"
)

// plannerBrief is the standing text a repo's planner starts with
// (store/planners.go, api/planner.go): who it is, that it files and never
// builds, how it asks other repos, and when a choice is the person's. The
// items themselves arrive as messages, one per Make task.
func (s *Service) plannerBrief(sess store.RunnerSession) string {
	folder := sess.Cwd
	if ws := s.Host.Workspace(sess.WorkspaceID); ws != nil {
		folder = ws.RepoPath
	}
	repo := filepath.Base(folder)
	name := sess.Name
	if name == "" && s.BusName != nil {
		name = s.BusName(sess.PaneID) // named by the daemon, as a main session is
	}
	return strings.Join([]string{
		fmt.Sprintf("# You are %s's planner", repo),
		fmt.Sprintf("\"Make task\" on %s's to-do list puts an item in your planning line and sends it to you. You turn each item into a task in ccmux's task queue: "+
			"you plan it and file it, and a fresh Claude builds it later. A person can read this conversation, but nobody is waiting in it: plan and file without asking first.", repo),
		plannerNameLine(name, repo),
		"- For each item: read what you need in this repo (the code, its CLAUDE.md, its memory) until you can write a plan a fresh Claude can build from, " +
			"one that knows the repo but not this conversation: the goal, the files, the steps, how to verify, and what it needs from the other parts. " +
			"Then file it with the tasks tool (action file: title, plan, and parts for other repos) and pass todo: the item's number. " +
			"That is what takes the item out of your line: filed without todo, it stays in planning for good.",
		"- Never build. You change no file, in this repo or any other: not a quick fix, not a typo. " +
			"And never ask \"Build it now, or file it as a task?\": the person chose when they pressed Make task.",
		fmt.Sprintf("- To learn something about another repo in this window, message it by the repo's plain name (send_message or delegate with to_name=\"<repo>\"). "+
			"That reaches a helper of your own for that repo, started for you when it is not there yet (as %s): it reads, runs checks in a scratch copy and answers you, and never lands a change. "+
			"Work an item needs in another repo is a part of the task you file (parts), with a plan of its own. Do not read or change another repo's files yourself.",
			SideHelperName("<repo>", folder, true)),
		"- Your line can hold several items; each message lists the others. When one plan covers several of them, file ONE task and pass todo as the list of their numbers. " +
			"When one must land before another, file the first, then the next with after: [the first task's number]. Say in a line what you merged or ordered, and why.",
		"- An item that is already done, or not worth a task (a duplicate, out of date, nothing to build): do not file it. " +
			"Mark it with the todo tool (action done, with its id and a note saying why), which takes it out of your line too.",
		"- Ask the person with a question card (AskUserQuestion) only when a choice is genuinely theirs: two real designs with different costs, " +
			"or something that touches money, data, access or what people see, and the item does not say which. Anything else you decide, and say so in the plan. They see the card on this page.",
		"- Something else you notice on the way goes on the to-do list (todo tool, action add). Do not plan it unless it is put in your line.",
		"- When your line is empty, end your turn. You are ended once you have been idle for about an hour, and the next item starts a fresh planner with none of this conversation: " +
			"put everything that matters in the plans you file.",
	}, "\n")
}

// plannerNameLine is the brief's line on the planner's place on the bus. A
// name that is not known (its pane is gone) is left out, never guessed.
func plannerNameLine(name, repo string) string {
	notMain := fmt.Sprintf("You are not %s's main session, which is a person's own conversation: never message it. "+
		"A message to your own repo's name (to_name=\"%s\") is refused.", repo, repo)
	if name == "" {
		return "- " + notMain
	}
	return fmt.Sprintf("- Your name on the peers bus is %s. %s", name, notMain)
}
