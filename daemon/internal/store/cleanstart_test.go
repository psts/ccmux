package store

import (
	"errors"
	"fmt"
	"path/filepath"
	"testing"
)

func TestACleanStartMovesTheConversationAndListsTheOneItLeft(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "reg.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if err := st.AddRunnerSession(RunnerSession{ID: "s1", Cwd: "/r", Wanted: SessionRunning, Kind: SessionLead, WorkspaceID: "w", CreatedAt: 1}); err != nil {
		t.Fatal(err)
	}
	if err := st.StartCleanConversation("s1", LeftConversation{ID: "s1", At: 10, Key: "k1"}, "c2", 10); err != nil {
		t.Fatal(err)
	}
	got, _ := st.RunnerSession("s1")
	if got.ConversationID() != "c2" || len(got.Left) != 1 || got.Left[0] != (LeftConversation{ID: "s1", At: 10, Key: "k1"}) {
		t.Fatalf("after one clean start: conv %q, left %+v", got.ConversationID(), got.Left)
	}
	if !got.HasLeft("s1") || got.HasLeft("c2") || !got.LeftFor("k1") || got.LeftFor("k2") || got.LeftFor("") {
		t.Fatalf("the list's questions: %+v", got.Left)
	}
	if err := st.StartCleanConversation("s1", LeftConversation{ID: "c2", At: 20, Key: "k2"}, "c3", 20); err != nil {
		t.Fatal(err)
	}
	got, _ = st.RunnerSession("s1")
	if got.ConversationID() != "c3" || len(got.Left) != 2 || got.Left[1].ID != "c2" || got.LeftFor("k1") {
		t.Fatalf("a second one goes after the first, and only the newest answers for its key: %+v", got.Left)
	}
	if err := st.StartCleanConversation("nope", LeftConversation{ID: "x"}, "y", 1); !errors.Is(err, ErrNoSession) {
		t.Fatalf("no row: %v", err)
	}
}

func TestTheLeftListKeepsTheNewestAndSurvivesABadColumn(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "reg.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if err := st.AddRunnerSession(RunnerSession{ID: "s1", Cwd: "/r", Wanted: SessionRunning, CreatedAt: 1}); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < leftCap+3; i++ {
		if err := st.StartCleanConversation("s1", LeftConversation{ID: fmt.Sprint("c", i), At: int64(i)}, fmt.Sprint("c", i+1), int64(i)); err != nil {
			t.Fatal(err)
		}
	}
	got, _ := st.RunnerSession("s1")
	if len(got.Left) != leftCap || got.Left[0].ID != "c3" || got.Left[leftCap-1].ID != fmt.Sprint("c", leftCap+2) {
		t.Fatalf("capped to the newest %d: first %+v, %d kept", leftCap, got.Left[0], len(got.Left))
	}
	if _, err := st.db.Exec(`UPDATE sessions SET left_conversations='not json' WHERE id='s1'`); err != nil {
		t.Fatal(err)
	}
	got, err = st.RunnerSession("s1")
	if err != nil || got == nil || got.Left != nil {
		t.Fatalf("an unreadable list leaves the row readable, with none: %+v %v", got, err)
	}
}
