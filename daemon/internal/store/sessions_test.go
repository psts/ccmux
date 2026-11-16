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
