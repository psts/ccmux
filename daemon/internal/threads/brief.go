package threads

import (
	"fmt"
	"log"
	"path/filepath"
	"strings"

	"ccmux.dev/ccmuxd/internal/store"
	"ccmux.dev/ccmuxd/internal/taskq"
)

// Brief is the standing text a thread member starts with, on top of Claude
// Code's own system prompt (runner.Config.Brief): who it is in its thread
// and how a thread works. "" for a session in no thread. It is written for
// a member a person talks to live, not for the task queue's unattended
// worker: it may ask, and it reports to its lead.
func (s *Service) Brief(sess store.RunnerSession) string {
	if sess.ThreadID == 0 {
		return ""
	}
	th, err := s.St.Thread(sess.ThreadID)
	if err != nil || th == nil {
		log.Printf("threads: session %s starts without its brief, thread #%d unreadable: %v", sess.ID, sess.ThreadID, err)
		return ""
	}
	if sess.Role == store.RoleLead {
		return leadBrief(*th, sess)
	}
	return helperBrief(*th, sess, s.leadName(*th))
}

// leadName is the bus name of a thread's lead, for its helpers' briefs.
func (s *Service) leadName(th store.Thread) string {
	lead, err := s.St.RunnerSession(th.LeadSession)
	if err != nil || lead == nil {
		return "the thread's lead"
	}
	return lead.Name
}

func titleOf(th store.Thread) string {
	if th.Title == "" {
		return "(not named yet)"
	}
	return th.Title
}

// where is the brief's line on where a member works now.
func where(sess store.RunnerSession) string {
	if sess.ReadOnly || sess.Worktree == "" {
		return fmt.Sprintf("You are in %s, the repo's own folder, where you only read: a person or the repo's own session may be working there. "+
			"Your first change (a file edit, or a command that does more than read) moves you into your own git worktree on the thread's branch: "+
			"that one call is refused, the session restarts in the worktree, and you make the change there.", sess.Repo)
	}
	return fmt.Sprintf("You work in your own git worktree, %s, on branch %s. Never change %s, the repo's own folder: every file there has its copy here.",
		sess.Worktree, sess.Branch, sess.Repo)
}

// workRules are what every member keeps to once it changes things.
const workRules = "Follow the repo's CLAUDE.md. Commit your work on your thread branch and leave the repo's checks green. " +
	"Do not push, merge, rebase onto another branch or switch branches, and change nothing under .claude/ (a branch that does is not merged). " +
	"If something reviews your commit in the background, wait for it and deal with what it finds."

func leadBrief(th store.Thread, sess store.RunnerSession) string {
	repo := filepath.Base(sess.Repo)
	return strings.Join([]string{
		fmt.Sprintf("# You lead ccmux thread #%d: %s", th.ID, titleOf(th)),
		fmt.Sprintf("A thread is one piece of work with its own team: you, in %s, and a helper of your own in every other repo the work needs. A person reads this thread and answers you here.", repo),
		fmt.Sprintf("- Your name on the peers bus is %s.", sess.Name),
		fmt.Sprintf("- To get work, or an answer, from another repo in this window, message it by the repo's plain name (send_message or delegate with to_name=\"<repo>\"). "+
			"In this thread that name reaches the thread's OWN helper for that repo, started for you when it is not there yet (as %s), never the repo's own session. "+
			"Plain questions about another repo go the same way. Do not read or change another repo's files yourself.", taskq.ThreadBusName("<repo>", th.ID)),
		"- " + where(sess),
		"- " + workRules,
		"- The thread tool is yours: action \"helpers\" lists your helpers and where each works; \"stand_down\" with repo ends one helper; " +
			"\"merge\" lands every member's branch (each is rebased onto its repo's trunk, the repo's checks run, and all of them fast-forward or none does); " +
			"\"end\" stands every helper down and closes the thread.",
		"- Merge when the work is done: yours and every helper's is committed and its reviews are dealt with. The merge takes minutes and answers with a message, " +
			"so end your turn after asking for it and change nothing meanwhile. A merge that fails leaves the thread open and says why: fix it, or pass it to the helper it names, then merge again.",
		"- When a choice is the person's, ask them with a question card (AskUserQuestion). They see it in this thread.",
	}, "\n")
}

func helperBrief(th store.Thread, sess store.RunnerSession, lead string) string {
	repo := filepath.Base(sess.Repo)
	return strings.Join([]string{
		fmt.Sprintf("# You are a helper in ccmux thread #%d: %s", th.ID, titleOf(th)),
		fmt.Sprintf("A thread is one piece of work with its own team. %s leads it and started you to work in %s for it: the lead owns the thread, you own this repo's part of it.", lead, repo),
		fmt.Sprintf("- Your name on the peers bus is %s. The lead is %s. A message to another repo's plain name (to_name=\"<repo>\") reaches the thread's own helper there, "+
			"started when it is not there yet; anyone else on the bus you reach by id, as always.", sess.Name, lead),
		"- What the lead wants reaches you as messages. Many are plain questions: answer those from reading, with send_message to the lead " +
			"(or update_task when the message is a delegation). Say what you found or did, and where. Never make the lead ask twice.",
		"- " + where(sess),
		"- " + workRules,
		"- When your part is done, tell the lead: what you built, on which commit, and how you checked it. The lead merges the whole thread and stands you down; you do not end yourself.",
		"- Need a decision? Ask the lead first. When only the person can decide, raise a question card (AskUserQuestion): they see it in the thread.",
	}, "\n")
}
