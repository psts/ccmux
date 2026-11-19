package store

import (
	"errors"
	"path/filepath"
	"testing"
)

func agentRow(id, home string) RunnerSession {
	return RunnerSession{ID: id, Name: "radar", Cwd: "/w/agents/radar", Wanted: SessionRunning, PaneID: "pane-" + home, WorkspaceID: home, Kind: SessionAgent, CreatedAt: 1}
}

func TestAnAgentsHomeHasOneSessionThatHasNotEnded(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "reg.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if err := st.AddRunnerSession(agentRow("a1", "home")); err != nil {
		t.Fatal(err)
	}
	// A click and a message start the same agent at once: one wins.
	if err := st.AddRunnerSession(agentRow("a2", "home")); !errors.Is(err, ErrAgentExists) {
		t.Fatalf("a second session for the same home: %v", err)
	}
	if err := st.AddRunnerSession(agentRow("b1", "other-home")); err != nil {
		t.Fatalf("another home's agent is its own: %v", err)
	}
	// A thread of the same workspace id is no agent session, and is not counted.
	if err := st.AddRunnerSession(RunnerSession{ID: "t1", Cwd: "/r", Wanted: SessionRunning, WorkspaceID: "home"}); err != nil {
		t.Fatal(err)
	}
	got, err := st.AgentSession("home")
	if err != nil || got == nil || got.ID != "a1" {
		t.Fatalf("the home's session: %+v %v", got, err)
	}
	// Ended, it frees the home: the next start makes a new one there.
	if err := st.SetRunnerSessionWanted("a1", SessionDone, 2); err != nil {
		t.Fatal(err)
	}
	if got, _ := st.AgentSession("home"); got != nil {
		t.Fatalf("an ended session is not the home's: %+v", got)
	}
	if err := st.AddRunnerSession(agentRow("a3", "home")); err != nil {
		t.Fatalf("a new session after the last ended: %v", err)
	}
	if list, _ := st.AgentSessions(); len(list) != 2 {
		t.Fatalf("agent sessions that have not ended: %+v", list)
	}
}

func TestAnAgentsConversationAndItsFreshMark(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "reg.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if err := st.AddRunnerSession(agentRow("a1", "home")); err != nil {
		t.Fatal(err)
	}
	row, _ := st.RunnerSession("a1")
	if row.ConversationID() != "a1" || row.FreshNext {
		t.Fatalf("a new session runs its own conversation: %+v", row)
	}
	if err := st.SleepRunnerSession("a1", true, 3); err != nil {
		t.Fatal(err)
	}
	if row, _ = st.RunnerSession("a1"); row.Wanted != SessionAsleep || !row.FreshNext {
		t.Fatalf("asleep and marked in one write: %+v", row)
	}
	if err := st.RotateRunnerConversation("a1", "c2", 4); err != nil {
		t.Fatal(err)
	}
	if row, _ = st.RunnerSession("a1"); row.ConversationID() != "c2" || row.FreshNext {
		t.Fatalf("the new conversation, and the mark cleared: %+v", row)
	}
	if err := st.SetRunnerSessionAgentVersion("a1", "1.2.0"); err != nil {
		t.Fatal(err)
	}
	if err := st.SetRunnerSessionFresh("a1", true, 5); err != nil {
		t.Fatal(err)
	}
	if row, _ = st.RunnerSession("a1"); row.AgentVersion != "1.2.0" || !row.FreshNext {
		t.Fatalf("version and mark kept: %+v", row)
	}
	if err := st.SetRunnerSessionFresh("nobody", true, 5); !errors.Is(err, ErrNoSession) {
		t.Fatalf("a session with no row: %v", err)
	}
}
