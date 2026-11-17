package threads

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"ccmux.dev/ccmuxd/internal/model"
	"ccmux.dev/ccmuxd/internal/store"
)

func TestAThreadStartsWithItsLeadReadingInTheReposFolder(t *testing.T) {
	r := newRig(t)
	th, lead := r.thread("Trade CSV export")
	if th.ID == 0 || th.State != store.ThreadOpen || th.Group != "w1" || th.LeadSession != lead.ID || th.Model != "haiku" {
		t.Fatalf("the thread knows its window, its lead and its model: %+v", th)
	}
	if got, _ := r.st.Thread(th.ID); got == nil || got.LeadSession != lead.ID {
		t.Fatalf("and its row does too: %+v", got)
	}
	want := store.RunnerSession{Name: "app-thread-1", Cwd: r.repo("app"), Model: "haiku", WorkspaceID: "ws-app", Title: "Trade CSV export",
		ThreadID: th.ID, Role: store.RoleLead, Repo: r.repo("app"), ReadOnly: true}
	got := r.row(lead.ID)
	got.ID, got.PaneID, got.Wanted, got.CreatedAt, got.UpdatedAt = "", "", "", 0, 0
	if got != want {
		t.Fatalf("the lead starts in its repo's own folder, read only, under its thread's name:\n got %+v\nwant %+v", got, want)
	}
}

func TestAThreadNeedsARepoSessionInAGitRepo(t *testing.T) {
	r := newRig(t)
	if _, _, err := r.svc.Create(New{WorkspaceID: "nope"}); err == nil || !strings.Contains(err.Error(), "not a repo session") {
		t.Fatalf("an unknown session leads nothing: %v", err)
	}
	r.host.list = append(r.host.list, &model.Workspace{ID: "ws-agent", Agent: "radar", RepoPath: r.repo("app")})
	if _, _, err := r.svc.Create(New{WorkspaceID: "ws-agent"}); err == nil {
		t.Fatal("a role agent's session leads no thread")
	}
	plain := filepath.Join(r.root, "notes")
	os.MkdirAll(plain, 0o755)
	r.host.list = append(r.host.list, &model.Workspace{ID: "ws-notes", RepoPath: plain})
	if _, _, err := r.svc.Create(New{WorkspaceID: "ws-notes"}); err == nil || !strings.Contains(err.Error(), "not a git repo") {
		t.Fatalf("a folder that is no git repo leads no thread: %v", err)
	}
	if list, _ := r.st.Threads(); len(list) != 0 {
		t.Fatalf("a refused thread leaves no row: %+v", list)
	}
}

func TestAThreadWhoseLeadCannotStartIsEnded(t *testing.T) {
	r := newRig(t)
	r.sess.createErr = errors.New("no systemd")
	th, _, err := r.svc.Create(New{WorkspaceID: "ws-app", Title: "x"})
	if err == nil || !strings.Contains(err.Error(), "no systemd") {
		t.Fatalf("the caller hears why: %v", err)
	}
	if got, _ := r.st.Thread(th.ID); got == nil || got.State != store.ThreadEnded || !strings.Contains(got.Note, "no systemd") {
		t.Fatalf("nothing leads it, so it is ended with the reason: %+v", got)
	}
}

func TestAMembersFirstChangeMovesItIntoItsWorktree(t *testing.T) {
	r := newRig(t)
	th, lead := r.thread("Trade CSV export")
	moved, err := r.svc.FirstEdit(lead.ID)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"app thread-1 thread/1-trade-csv-export main"}; strings.Join(r.git.added, "|") != strings.Join(want, "|") {
		t.Fatalf("its worktree is cut from the trunk on the thread's branch: %v", r.git.added)
	}
	got := r.row(lead.ID)
	if got.ReadOnly || got.Cwd != got.Worktree || !strings.HasSuffix(got.Worktree, "thread-1") || got.Branch != "thread/1-trade-csv-export" || got.Repo != r.repo("app") {
		t.Fatalf("its row says where it works now: %+v", got)
	}
	for _, text := range []string{moved.Message, moved.Notice} {
		if !strings.Contains(text, got.Worktree) || !strings.Contains(text, got.Branch) || !strings.Contains(text, "made from main") {
			t.Fatalf("it is told its worktree, branch and trunk: %q", text)
		}
	}
	if !strings.Contains(moved.Message, "was not run") || !strings.Contains(moved.Notice, "Go on with what you were about to change") {
		t.Fatalf("the refused call says it was refused, the notice says to go on: %+v", moved)
	}
	// Claude sent a second edit in the same message, before the restart:
	// same answer, no second worktree, and one restart for both.
	again, err := r.svc.FirstEdit(lead.ID)
	if err != nil || !strings.Contains(again.Message, got.Worktree) {
		t.Fatalf("asked again it is told where it is: %+v %v", again, err)
	}
	eventually(t, "the member restarts in its worktree", func() bool { return len(r.sess.moves()) == 1 })
	time.Sleep(4 * r.svc.MoveAfter)
	if len(r.git.added) != 1 || len(r.sess.moves()) != 1 {
		t.Fatalf("one worktree, one restart: %v %v", r.git.added, r.sess.moves())
	}
	_ = th
}

func TestARestartThatFailsIsSaidToTheMemberAndItsNextChangeTriesAgain(t *testing.T) {
	r := newRig(t)
	_, lead := r.thread("x")
	_, other := r.thread("y")
	r.sess.failMoves(errors.New("systemd refused"))
	if _, err := r.svc.FirstEdit(lead.ID); err != nil {
		t.Fatal(err)
	}
	eventually(t, "the member hears its restart failed", func() bool { return len(r.sess.prompts(lead.ID)) == 1 })
	if said := r.sess.prompts(lead.ID)[0]; !strings.Contains(said, "systemd refused") || !strings.Contains(said, "still runs in the repo's own folder") {
		t.Fatalf("it is told why, and where it still is: %q", said)
	}
	if len(r.sess.prompts(other.ID)) != 0 {
		t.Fatalf("a member of another thread hears nothing: %v", r.sess.prompts(other.ID))
	}
	// Its runner is still fenced, so its next change asks again: that is
	// the retry, and no second worktree is made for it.
	r.sess.failMoves(nil)
	if _, err := r.svc.FirstEdit(lead.ID); err != nil {
		t.Fatal(err)
	}
	eventually(t, "the next ask restarts it", func() bool { return len(r.sess.moves()) == 1 })
	if len(r.git.added) != 1 || len(r.sess.prompts(lead.ID)) != 1 {
		t.Fatalf("one worktree, and nothing more said once it worked: %v %v", r.git.added, r.sess.prompts(lead.ID))
	}
}

func TestAMergedThreadIsOpenAgainOnceAMemberHasNewWork(t *testing.T) {
	r := newRig(t)
	merged, lead := r.thread("x")
	idle, _ := r.thread("y")
	for _, th := range []store.Thread{merged, idle} {
		th.State = store.ThreadMerged
		if err := r.st.SaveThread(th); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := r.svc.FirstEdit(lead.ID); err != nil {
		t.Fatal(err)
	}
	if got, _ := r.st.Thread(merged.ID); got.State != store.ThreadOpen {
		t.Fatalf("a thread whose member changes something again is open again: %+v", got)
	}
	if got, _ := r.st.Thread(idle.ID); got.State != store.ThreadMerged {
		t.Fatalf("another merged thread stays merged: %+v", got)
	}
}

func TestAWorktreeThatCannotBeMadeLeavesTheMemberReading(t *testing.T) {
	r := newRig(t)
	_, lead := r.thread("x")
	r.git.err = errors.New("disk full")
	if _, err := r.svc.FirstEdit(lead.ID); err == nil || !strings.Contains(err.Error(), "disk full") {
		t.Fatalf("the member hears why: %v", err)
	}
	if got := r.row(lead.ID); !got.ReadOnly || got.Cwd != r.repo("app") || len(r.sess.moves()) != 0 {
		t.Fatalf("it stays where it was, read only, and is not restarted: %+v", got)
	}
}

func TestOnlyAThreadMemberIsMoved(t *testing.T) {
	r := newRig(t)
	if err := r.st.AddRunnerSession(store.RunnerSession{ID: "plain", Cwd: r.repo("app"), Wanted: store.SessionRunning}); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"plain", "nope"} {
		if _, err := r.svc.FirstEdit(id); !errors.Is(err, ErrNotMember) {
			t.Fatalf("%s is in no thread: %v", id, err)
		}
	}
}

func TestABranchIsNamedAfterWhatTheLeadWasFirstAsked(t *testing.T) {
	r := newRig(t)
	_, lead := r.thread("") // made by the page before its first message
	if err := r.st.SetRunnerSessionTitle(lead.ID, "Fix the login page", 2); err != nil {
		t.Fatal(err)
	}
	if _, err := r.svc.FirstEdit(lead.ID); err != nil {
		t.Fatal(err)
	}
	if got := r.row(lead.ID); got.Branch != "thread/1-fix-the-login-page" {
		t.Fatalf("an unnamed thread takes its lead's title: %q", got.Branch)
	}
}

func TestAMemberStartsWithABriefThatFitsItsRoleAndPlace(t *testing.T) {
	r := newRig(t)
	th, lead := r.thread("Trade CSV export")
	text := r.svc.Brief(r.row(lead.ID))
	for _, want := range []string{"You lead ccmux thread #1: Trade CSV export", "app-thread-1", "to_name=\"<repo>\"", "<repo>-thread-1", "where you only read", `"merge"`, "AskUserQuestion"} {
		if !strings.Contains(text, want) {
			t.Fatalf("the lead's brief lacks %q:\n%s", want, text)
		}
	}
	r.svc.FirstEdit(lead.ID)
	text = r.svc.Brief(r.row(lead.ID))
	if strings.Contains(text, "where you only read") || !strings.Contains(text, "You work in your own git worktree") || !strings.Contains(text, "thread/1-trade-csv-export") {
		t.Fatalf("once moved, the brief says where it works:\n%s", text)
	}
	helper := store.RunnerSession{ID: "h1", Name: "backend-thread-1", Cwd: r.repo("backend"), Repo: r.repo("backend"), ThreadID: th.ID, Role: store.RoleHelper, ReadOnly: true}
	text = r.svc.Brief(helper)
	for _, want := range []string{"You are a helper in ccmux thread #1", "app-thread-1 leads it", "backend-thread-1", "you do not end yourself", "Ask the lead first"} {
		if !strings.Contains(text, want) {
			t.Fatalf("the helper's brief lacks %q:\n%s", want, text)
		}
	}
	if strings.Contains(text, "thread tool") {
		t.Fatalf("the thread tool is the lead's, not a helper's:\n%s", text)
	}
	if r.svc.Brief(store.RunnerSession{ID: "plain"}) != "" || r.svc.Brief(store.RunnerSession{ID: "x", ThreadID: 99}) != "" {
		t.Fatal("a session in no thread, or in one that is gone, has no brief")
	}
}
