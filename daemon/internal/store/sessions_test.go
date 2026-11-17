package store

import (
	"errors"
	"path/filepath"
	"testing"
)

func openSessions(t *testing.T) *SQLite {
	t.Helper()
	st, err := Open(filepath.Join(t.TempDir(), "reg.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

func TestRunnerSessionRowsKeepWhatShouldRun(t *testing.T) {
	st := openSessions(t)
	if err := st.AddRunnerSession(RunnerSession{ID: "s1", Cwd: "/r/app", Wanted: SessionRunning, CreatedAt: 1, UpdatedAt: 1}); err != nil {
		t.Fatal(err)
	}
	if err := st.AddRunnerSession(RunnerSession{ID: "s2", Cwd: "/r/api", Wanted: SessionRunning, CreatedAt: 2, UpdatedAt: 2}); err != nil {
		t.Fatal(err)
	}
	if err := st.SetRunnerSessionModel("s1", "haiku", 3); err != nil {
		t.Fatal(err)
	}
	if err := st.SetRunnerSessionWanted("s2", SessionDone, 4); err != nil {
		t.Fatal(err)
	}
	list, err := st.RunnerSessions()
	if err != nil || len(list) != 1 || list[0].ID != "s1" || list[0].Model != "haiku" {
		t.Fatalf("a done session drops out of the list: %+v %v", list, err)
	}
	if got, _ := st.RunnerSession("s2"); got == nil || got.Wanted != SessionDone {
		t.Fatalf("the done row is still readable by id: %+v", got)
	}
	if err := st.SetRunnerSessionWanted("nope", SessionDone, 5); !errors.Is(err, ErrNoSession) {
		t.Fatalf("an unknown session is an error, got %v", err)
	}
}

func TestInboxKeepsOrderUntilDelivered(t *testing.T) {
	st := openSessions(t)
	for _, id := range []string{"p1", "p2", "p3"} {
		if err := st.AddInboxPromptAndWake(InboxPrompt{ID: id, SessionID: "s1", Text: "do " + id, CreatedAt: 1}, false, 1); err != nil {
			t.Fatal(err)
		}
	}
	if err := st.AddInboxPromptAndWake(InboxPrompt{ID: "q1", SessionID: "s2", Text: "other", CreatedAt: 1}, false, 1); err != nil {
		t.Fatal(err)
	}
	if err := st.MarkPromptsDelivered([]string{"p1", "p3"}, 9); err != nil {
		t.Fatal(err)
	}
	open, err := st.UndeliveredPrompts("s1")
	if err != nil || len(open) != 1 || open[0].ID != "p2" || open[0].Seq != 2 {
		t.Fatalf("only p2 waits, in its place: %+v %v", open, err)
	}
	if err := st.MarkPromptsDelivered(nil, 9); err != nil {
		t.Fatalf("nothing to mark is not an error: %v", err)
	}
}

func TestCardsOpenOnceAndExpire(t *testing.T) {
	st := openSessions(t)
	c := SessionCard{ID: "abcde", SessionID: "s1", Kind: "permission", Body: `{"tool":"Bash"}`, Boot: "b1", CreatedAt: 1}
	if err := st.UpsertSessionCard(c); err != nil {
		t.Fatal(err)
	}
	c.Body, c.CreatedAt = `{"changed":true}`, 7
	if err := st.UpsertSessionCard(c); err != nil {
		t.Fatal(err)
	}
	cards, _ := st.OpenSessionCards("s1")
	if len(cards) != 1 || cards[0].CreatedAt != 1 || cards[0].Boot != "b1" {
		t.Fatalf("a card seen again keeps its first record: %+v", cards)
	}
	if err := st.SetSessionCardState("s1", "abcde", CardExpired, 8); err != nil {
		t.Fatal(err)
	}
	if err := st.UpsertSessionCard(c); err != nil {
		t.Fatal(err)
	}
	if cards, _ := st.OpenSessionCards("s1"); len(cards) != 0 {
		t.Fatalf("an expired card does not reopen: %+v", cards)
	}
}

func TestAPromptAndItsWakeLandTogether(t *testing.T) {
	st := openSessions(t)
	st.AddRunnerSession(RunnerSession{ID: "s1", Cwd: "/r", Wanted: SessionAsleep, Note: "put to sleep: crashed", CreatedAt: 1})
	st.AddRunnerSession(RunnerSession{ID: "s2", Cwd: "/r", Wanted: SessionAsleep, CreatedAt: 1})
	if err := st.AddInboxPromptAndWake(InboxPrompt{ID: "p1", SessionID: "s1", Text: "go", CreatedAt: 2}, true, 3); err != nil {
		t.Fatal(err)
	}
	got, _ := st.RunnerSession("s1")
	if got.Wanted != SessionRunning || got.Note != "" {
		t.Fatalf("woken, note cleared: %+v", got)
	}
	if err := st.AddInboxPromptAndWake(InboxPrompt{ID: "p1", SessionID: "s2", Text: "dup id", CreatedAt: 2}, true, 3); err == nil {
		t.Fatal("a prompt that cannot be queued is an error")
	}
	if other, _ := st.RunnerSession("s2"); other.Wanted != SessionAsleep {
		t.Fatalf("and wakes nothing: %+v", other)
	}
	if open, _ := st.UndeliveredPrompts("s2"); len(open) != 0 {
		t.Fatalf("and queues nothing: %+v", open)
	}
}

func TestASessionRowKeepsItsPaneAndItsAttention(t *testing.T) {
	st := openSessions(t)
	if err := st.AddRunnerSession(RunnerSession{ID: "s1", Cwd: "/r/app", Wanted: SessionRunning, PaneID: "p1", WorkspaceID: "ws1", CreatedAt: 1}); err != nil {
		t.Fatal(err)
	}
	st.AddRunnerSession(RunnerSession{ID: "s2", Cwd: "/r/app", Wanted: SessionDone, PaneID: "p2", WorkspaceID: "ws1", CreatedAt: 2})
	st.AddRunnerSession(RunnerSession{ID: "s3", Cwd: "/r/old", Wanted: SessionRunning, CreatedAt: 3}) // from before panes
	if err := st.SaveRunnerAttention("p1", "needs_input", "permission", 77); err != nil {
		t.Fatal(err)
	}
	got, _ := st.RunnerSession("s1")
	if got.PaneID != "p1" || got.WorkspaceID != "ws1" || got.Attention != "needs_input" || got.AttentionReason != "permission" || got.AttentionSince != 77 {
		t.Fatalf("pane, workspace and attention come back with the row: %+v", got)
	}
	// What the supervisor writes leaves the attention alone, and the other way round.
	st.SetRunnerSessionWanted("s1", SessionAsleep, 9)
	st.SetRunnerSessionModel("s1", "haiku", 9)
	if got, _ := st.RunnerSession("s1"); got.Attention != "needs_input" || got.Wanted != SessionAsleep || got.Model != "haiku" {
		t.Fatalf("the two writers do not overwrite each other: %+v", got)
	}
	if err := st.SaveRunnerAttention("no-such-pane", "idle", "", 0); !errors.Is(err, ErrNoSession) {
		t.Fatalf("a pane with no session row says so: %v", err)
	}
	ids, err := st.RunnerPaneIDs()
	if err != nil || len(ids) != 2 {
		t.Fatalf("every pane id a session ever had, ended ones too, and none for a session without one: %v %v", ids, err)
	}
	if old, _ := st.RunnerSession("s3"); old.PaneID != "" || old.Attention != "" {
		t.Fatalf("a row from before panes reads as having none: %+v", old)
	}
}

func TestSessionPaneColumnsAreAddedToAnOlderRegistry(t *testing.T) {
	path := filepath.Join(t.TempDir(), "reg.db")
	st, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	// A registry written by the release before panes: the table without the
	// new columns, holding a session.
	for _, q := range []string{
		`DROP TABLE sessions`,
		`CREATE TABLE sessions (id TEXT PRIMARY KEY, name TEXT DEFAULT '', cwd TEXT NOT NULL, model TEXT DEFAULT '',
  wanted TEXT NOT NULL, note TEXT DEFAULT '', created_at INTEGER, updated_at INTEGER)`,
		`INSERT INTO sessions (id,cwd,wanted,created_at,updated_at) VALUES ('old','/r/app','running',1,1)`,
	} {
		if _, err := st.db.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	st.Close()
	st, err = Open(path)
	if err != nil {
		t.Fatalf("an older registry opens: %v", err)
	}
	defer st.Close()
	list, err := st.RunnerSessions()
	if err != nil || len(list) != 1 || list[0].ID != "old" || list[0].PaneID != "" {
		t.Fatalf("its session reads back, with no pane: %+v %v", list, err)
	}
	if err := st.AddRunnerSession(RunnerSession{ID: "new", Cwd: "/r/app", Wanted: SessionRunning, PaneID: "p9", WorkspaceID: "ws"}); err != nil {
		t.Fatalf("and a new one with a pane is stored: %v", err)
	}
}

func TestHasInboxPromptKnowsDeliveredOnesToo(t *testing.T) {
	st := openSessions(t)
	st.AddRunnerSession(RunnerSession{ID: "s1", Cwd: "/r", Wanted: SessionRunning, CreatedAt: 1})
	if had, err := st.HasInboxPrompt("p1"); had || err != nil {
		t.Fatalf("not yet: %v %v", had, err)
	}
	st.AddInboxPromptAndWake(InboxPrompt{ID: "p1", SessionID: "s1", Text: "go", CreatedAt: 2}, false, 2)
	st.MarkPromptsDelivered([]string{"p1"}, 3)
	if had, err := st.HasInboxPrompt("p1"); !had || err != nil {
		t.Fatalf("a delivered prompt was still queued once: %v %v", had, err)
	}
}

func TestAPromptKeepsItsAttachments(t *testing.T) {
	st := openSessions(t)
	names := []string{"0123456789abcdef0123456789abcdef.png", "fedcba9876543210fedcba9876543210.jpg"}
	if err := st.AddInboxPromptAndWake(InboxPrompt{ID: "p1", SessionID: "s1", Text: "look", Attachments: names, CreatedAt: 1}, false, 1); err != nil {
		t.Fatal(err)
	}
	if err := st.AddInboxPromptAndWake(InboxPrompt{ID: "p2", SessionID: "s1", Text: "plain", CreatedAt: 1}, false, 1); err != nil {
		t.Fatal(err)
	}
	open, err := st.UndeliveredPrompts("s1")
	if err != nil || len(open) != 2 {
		t.Fatalf("%+v %v", open, err)
	}
	if got := open[0].Attachments; len(got) != 2 || got[0] != names[0] || got[1] != names[1] {
		t.Fatalf("the pictures come back with the prompt, in order: %v", got)
	}
	if open[1].Attachments != nil {
		t.Fatalf("a prompt with none has none: %v", open[1].Attachments)
	}
	// A value nobody could have written is said, not read as "no pictures".
	if _, err := st.db.Exec(`UPDATE session_inbox SET attachments='{oops' WHERE id='p1'`); err != nil {
		t.Fatal(err)
	}
	if _, err := st.UndeliveredPrompts("s1"); err == nil {
		t.Fatal("unreadable attachments must fail the read")
	}
}

func TestTheAttachmentsColumnIsAddedToAnOlderInbox(t *testing.T) {
	path := filepath.Join(t.TempDir(), "reg.db")
	st, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	// The inbox as v0.1.120 made it, holding a prompt nobody delivered yet.
	for _, q := range []string{
		`DROP TABLE session_inbox`,
		`CREATE TABLE session_inbox (id TEXT PRIMARY KEY, session_id TEXT NOT NULL, seq INTEGER NOT NULL, text TEXT NOT NULL,
  sender TEXT DEFAULT '', created_at INTEGER, delivered_at INTEGER DEFAULT 0)`,
		`INSERT INTO session_inbox (id,session_id,seq,text,created_at) VALUES ('old','s1',1,'from before',1)`,
	} {
		if _, err := st.db.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	st.Close()
	st, err = Open(path)
	if err != nil {
		t.Fatalf("an older registry opens: %v", err)
	}
	defer st.Close()
	open, err := st.UndeliveredPrompts("s1")
	if err != nil || len(open) != 1 || open[0].Text != "from before" || open[0].Attachments != nil {
		t.Fatalf("its waiting prompt reads back, with no pictures: %+v %v", open, err)
	}
	if err := st.AddInboxPromptAndWake(InboxPrompt{ID: "new", SessionID: "s1", Text: "x", Attachments: []string{"a.png"}, CreatedAt: 2}, false, 2); err != nil {
		t.Fatalf("and a new one with a picture is stored: %v", err)
	}
}

func TestAWorkspaceHasOneLeadThatHasNotEnded(t *testing.T) {
	st := openSessions(t)
	lead := RunnerSession{ID: "lead1", Cwd: "/r/app", Wanted: SessionRunning, WorkspaceID: "ws", Kind: SessionLead, Title: "app"}
	if err := st.AddRunnerSession(lead); err != nil {
		t.Fatal(err)
	}
	second := lead
	second.ID = "lead2"
	if err := st.AddRunnerSession(second); !errors.Is(err, ErrLeadExists) {
		t.Fatalf("a second lead for the same workspace: %v, want ErrLeadExists", err)
	}
	// What the rule must not stop: threads of that workspace, a lead of
	// another one, and sessions in no workspace at all.
	for _, ok := range []RunnerSession{
		{ID: "t1", Cwd: "/r/app", Wanted: SessionRunning, WorkspaceID: "ws", Title: "a thread"},
		{ID: "t2", Cwd: "/r/app", Wanted: SessionRunning, WorkspaceID: "ws"},
		{ID: "other", Cwd: "/r/api", Wanted: SessionRunning, WorkspaceID: "ws2", Kind: SessionLead},
		{ID: "bare1", Cwd: "/x", Wanted: SessionRunning, Kind: SessionLead},
		{ID: "bare2", Cwd: "/x", Wanted: SessionRunning, Kind: SessionLead},
	} {
		if err := st.AddRunnerSession(ok); err != nil {
			t.Fatalf("%s: %v", ok.ID, err)
		}
	}
	if got, err := st.RunnerLead("ws"); err != nil || got == nil || got.ID != "lead1" || got.Title != "app" || got.Kind != SessionLead {
		t.Fatalf("the workspace's lead: %+v %v", got, err)
	}
	if got, _ := st.RunnerLead("none"); got != nil {
		t.Fatalf("a workspace with no lead: %+v", got)
	}
	// An ended lead makes room for the next.
	if err := st.SetRunnerSessionWanted("lead1", SessionDone, 2); err != nil {
		t.Fatal(err)
	}
	if err := st.AddRunnerSession(second); err != nil {
		t.Fatalf("after the first ended: %v", err)
	}
	if got, _ := st.RunnerLead("ws"); got == nil || got.ID != "lead2" {
		t.Fatalf("the new lead: %+v", got)
	}
}

func TestASessionKeepsItsTitleAndGoal(t *testing.T) {
	st := openSessions(t)
	if err := st.AddRunnerSession(RunnerSession{ID: "s1", Cwd: "/r/app", Wanted: SessionRunning}); err != nil {
		t.Fatal(err)
	}
	if err := st.SetRunnerSessionTitle("s1", "Trade CSV export", 5); err != nil {
		t.Fatal(err)
	}
	if err := st.SetRunnerSessionGoal("s1", "Export closed trades. Next: the admin table.", 6); err != nil {
		t.Fatal(err)
	}
	got, _ := st.RunnerSession("s1")
	if got.Title != "Trade CSV export" || got.Goal != "Export closed trades. Next: the admin table." || got.Kind != "" || got.UpdatedAt != 6 {
		t.Fatalf("%+v", got)
	}
	if err := st.SetRunnerSessionTitle("nope", "x", 7); !errors.Is(err, ErrNoSession) {
		t.Fatalf("an unknown session: %v", err)
	}
	if err := st.SetRunnerSessionGoal("nope", "x", 7); !errors.Is(err, ErrNoSession) {
		t.Fatalf("an unknown session: %v", err)
	}
}

func TestThreadColumnsAreAddedToAnOlderRegistry(t *testing.T) {
	path := filepath.Join(t.TempDir(), "reg.db")
	st, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	// The sessions table as stage 2 left it: panes, no kind, title or goal.
	for _, q := range []string{
		`DROP INDEX sessions_one_lead`,
		`DROP TABLE sessions`,
		`CREATE TABLE sessions (id TEXT PRIMARY KEY, name TEXT DEFAULT '', cwd TEXT NOT NULL, model TEXT DEFAULT '',
  wanted TEXT NOT NULL, note TEXT DEFAULT '', created_at INTEGER, updated_at INTEGER, pane_id TEXT DEFAULT '',
  workspace_id TEXT DEFAULT '', attention TEXT DEFAULT '', attention_reason TEXT DEFAULT '', attention_since INTEGER DEFAULT 0)`,
		`INSERT INTO sessions (id,cwd,wanted,created_at,updated_at,pane_id,workspace_id) VALUES ('old','/r/app','running',1,1,'p1','ws')`,
	} {
		if _, err := st.db.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	st.Close()
	st, err = Open(path)
	if err != nil {
		t.Fatalf("an older registry opens: %v", err)
	}
	defer st.Close()
	list, err := st.RunnerSessions()
	if err != nil || len(list) != 1 || list[0].Kind != "" || list[0].Title != "" || list[0].PaneID != "p1" {
		t.Fatalf("its session reads back as a thread with no title: %+v %v", list, err)
	}
	lead := RunnerSession{ID: "lead", Cwd: "/r/app", Wanted: SessionRunning, WorkspaceID: "ws", Kind: SessionLead}
	if err := st.AddRunnerSession(lead); err != nil {
		t.Fatalf("and it takes a lead beside it: %v", err)
	}
	lead.ID = "again"
	if err := st.AddRunnerSession(lead); !errors.Is(err, ErrLeadExists) {
		t.Fatalf("the one-lead rule holds there too: %v", err)
	}
}

func TestAThreadKeepsItsRowAndItsMembers(t *testing.T) {
	st := openSessions(t)
	th, err := st.AddThread(Thread{Group: "w1", Title: "CSV export", State: ThreadOpen, Model: "haiku", CreatedAt: 1, UpdatedAt: 1})
	if err != nil || th.ID == 0 {
		t.Fatalf("a new thread gets an id: %+v %v", th, err)
	}
	lead := RunnerSession{ID: "s1", Cwd: "/r/app", Wanted: SessionRunning, CreatedAt: 2, ThreadID: th.ID, Role: RoleLead, Repo: "/r/app", ReadOnly: true}
	helper := RunnerSession{ID: "s2", Cwd: "/r/api", Wanted: SessionRunning, CreatedAt: 1, ThreadID: th.ID, Role: RoleHelper, Repo: "/r/api", ReadOnly: true}
	for _, r := range []RunnerSession{helper, lead} {
		if err := st.AddRunnerSession(r); err != nil {
			t.Fatal(err)
		}
	}
	th.LeadSession, th.State, th.Note, th.UpdatedAt = "s1", ThreadMerged, "merged app", 5
	if err := st.SaveThread(th); err != nil {
		t.Fatal(err)
	}
	got, err := st.Thread(th.ID)
	if err != nil || got == nil || *got != th {
		t.Fatalf("the thread reads back as saved: %+v %v", got, err)
	}
	members, err := st.ThreadMembers(th.ID)
	if err != nil || len(members) != 2 || members[0].ID != "s1" || members[1].ID != "s2" || !members[1].ReadOnly {
		t.Fatalf("the lead comes first, then the helpers: %+v %v", members, err)
	}
	if err := st.SaveThread(Thread{ID: 999, State: ThreadOpen}); !errors.Is(err, ErrNoThread) {
		t.Fatalf("a thread with no row is an error, got %v", err)
	}
	th.State = ThreadEnded
	if err := st.SaveThread(th); err != nil {
		t.Fatal(err)
	}
	if list, err := st.Threads(); err != nil || len(list) != 0 {
		t.Fatalf("an ended thread drops out of the list: %+v %v", list, err)
	}
}

func TestAThreadHasOneMemberPerRepoThatHasNotEnded(t *testing.T) {
	st := openSessions(t)
	add := func(id string, thread int64, repo string) error {
		return st.AddRunnerSession(RunnerSession{ID: id, Cwd: repo, Wanted: SessionRunning, ThreadID: thread, Role: RoleHelper, Repo: repo})
	}
	if err := add("h1", 7, "/r/api"); err != nil {
		t.Fatal(err)
	}
	if err := add("h2", 7, "/r/api"); !errors.Is(err, ErrMemberExists) {
		t.Fatalf("a second helper for the same repo is refused, got %v", err)
	}
	if err := add("h3", 8, "/r/api"); err != nil {
		t.Fatalf("another thread has its own helper there: %v", err)
	}
	if err := st.SetRunnerSessionWanted("h1", SessionDone, 2); err != nil {
		t.Fatal(err)
	}
	if err := add("h4", 7, "/r/api"); err != nil {
		t.Fatalf("a helper that was stood down makes room for a new one: %v", err)
	}
	for _, id := range []string{"p1", "p2"} {
		if err := st.AddRunnerSession(RunnerSession{ID: id, Cwd: "/r/api", Wanted: SessionRunning}); err != nil {
			t.Fatalf("sessions in no thread are not counted: %v", err)
		}
	}
}

func TestAMemberMovesIntoItsWorktreeAndBack(t *testing.T) {
	st := openSessions(t)
	if err := st.AddRunnerSession(RunnerSession{ID: "s1", Cwd: "/r/api", Wanted: SessionRunning, ThreadID: 3, Role: RoleHelper, Repo: "/r/api", ReadOnly: true}); err != nil {
		t.Fatal(err)
	}
	if err := st.SetRunnerSessionPlace("s1", SessionPlace{Cwd: "/wt/api/thread-3", Worktree: "/wt/api/thread-3", Branch: "thread/3-x"}, 4); err != nil {
		t.Fatal(err)
	}
	got, _ := st.RunnerSession("s1")
	if got.Cwd != "/wt/api/thread-3" || got.Worktree != "/wt/api/thread-3" || got.Branch != "thread/3-x" || got.ReadOnly || got.Repo != "/r/api" {
		t.Fatalf("it works in its worktree, and its repo is still its repo: %+v", got)
	}
	if err := st.SetRunnerSessionPlace("nope", SessionPlace{Cwd: "/x"}, 4); !errors.Is(err, ErrNoSession) {
		t.Fatalf("an unknown session is an error, got %v", err)
	}
}

func TestMemberColumnsAreAddedToAnOlderRegistry(t *testing.T) {
	path := filepath.Join(t.TempDir(), "reg.db")
	st, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	// A registry from before threads had helpers: the six columns and the
	// index are gone, the rows stay.
	for _, q := range []string{
		`DROP INDEX sessions_one_member`,
		`ALTER TABLE sessions DROP COLUMN thread_id`, `ALTER TABLE sessions DROP COLUMN role`, `ALTER TABLE sessions DROP COLUMN repo`,
		`ALTER TABLE sessions DROP COLUMN worktree`, `ALTER TABLE sessions DROP COLUMN branch`, `ALTER TABLE sessions DROP COLUMN read_only`,
		`INSERT INTO sessions (id,cwd,wanted,created_at,updated_at) VALUES ('old','/r/app','running',1,1)`,
	} {
		if _, err := st.db.Exec(q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	st.Close()
	st, err = Open(path)
	if err != nil {
		t.Fatalf("an older registry opens: %v", err)
	}
	defer st.Close()
	got, err := st.RunnerSession("old")
	if err != nil || got == nil || got.ThreadID != 0 || got.Role != "" || got.ReadOnly {
		t.Fatalf("an older row is in no thread: %+v %v", got, err)
	}
}
