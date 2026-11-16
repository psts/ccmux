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
