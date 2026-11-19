package runner

import (
	"errors"
	"slices"
	"strings"
	"testing"

	"ccmux.dev/ccmuxd/internal/store"
)

// agentRig is a rig whose daemon starts role agents: every start asks the
// base (start), and the test changes what it answers.
type agentRig struct {
	*rig
	start AgentStart
	asked int
}

func newAgentRig(t *testing.T) *agentRig {
	t.Helper()
	a := &agentRig{rig: newRig(t), start: AgentStart{
		Args:    []string{"--plugin-dir", "/agents/radar", "--system-prompt-file", "/agents/radar/AGENTS.md", "--setting-sources", "project", "--write-root", "/w/agents/radar"},
		Env:     map[string]string{"KB_TOKEN": "s3cret", "HOME": "/h2"},
		Model:   "sonnet",
		Version: "1.2.0",
	}}
	a.sup.cfg.Peers = "/bin/ccmux-peers"
	a.sup.cfg.Agent = func(store.RunnerSession) (AgentStart, error) {
		a.asked++
		return a.start, nil
	}
	return a
}

// agent is a role agent's session as the daemon makes it.
func agentSession() NewSession {
	return NewSession{Cwd: "/w/agents/radar", Name: "radar", Model: "opus", WorkspaceID: "home", Kind: store.SessionAgent, PermissionMode: "default", PaneID: "pane-radar"}
}

func TestAnAgentsSpecIsItsRunnersFlagsThenItsBases(t *testing.T) {
	a := newAgentRig(t)
	sess, err := a.sup.Create(agentSession())
	if err != nil {
		t.Fatal(err)
	}
	if sess.PaneID != "pane-radar" {
		t.Fatalf("the session's pane takes the id its home had: %q", sess.PaneID)
	}
	sp, args := argsOf(t, a.rig, sess.ID)
	want := []string{
		"--socket", a.paths.Socket(sess.ID), "--state", a.paths.StateFile(sess.ID), "--session-auto", sess.ID,
		"--detached", "1", "--setting-sources", "user,project,local", "--agent", "radar", "--media", a.paths.MediaDir(sess.ID),
		"--model", "sonnet", "--peers-command", "/bin/ccmux-peers", "--permission-mode", "default",
		"--plugin-dir", "/agents/radar", "--system-prompt-file", "/agents/radar/AGENTS.md", "--setting-sources", "project", "--write-root", "/w/agents/radar",
	}
	if !slices.Equal(sp.Args, want) {
		t.Fatalf("an agent's start line\n got %q\nwant %q", sp.Args, want)
	}
	// The base's model, not the row's; the SDK's own Claude Code, as in its
	// pane; no loadout and no brief of the daemon's.
	for _, never := range []string{"--model opus", "--claude", "--loadout", a.paths.BriefFile(sess.ID)} {
		if strings.Contains(args, never) {
			t.Errorf("an agent's start line must not say %q: %s", never, args)
		}
	}
	// The sidecar keeps the last --setting-sources it reads: an isolated
	// base's must come after the runner's own.
	if strings.LastIndex(args, "--setting-sources project") < strings.Index(args, "--setting-sources user,project,local") {
		t.Errorf("the base's setting sources must come last: %s", args)
	}
	if !slices.Equal(sp.Env, []string{"HOME=/h2", "KB_TOKEN=s3cret"}) {
		t.Errorf("its .env files' variables are its environment's, each name once: %q", sp.Env)
	}
	row, _ := a.st.RunnerSession(sess.ID)
	if row.AgentVersion != "1.2.0" {
		t.Errorf("its row keeps the base version it started with: %q", row.AgentVersion)
	}
}

// Every other session's spec is what it was before agents were sessions.
func TestAPersonsSpecIsUnchangedByAgents(t *testing.T) {
	a := newAgentRig(t)
	sess, _ := a.sup.Create(NewSession{Cwd: "/r/app", Model: "opus", WorkspaceID: "ws"})
	sp, _ := argsOf(t, a.rig, sess.ID)
	want := []string{
		"--socket", a.paths.Socket(sess.ID), "--state", a.paths.State + "/" + sess.ID + ".state.json", "--session-auto", sess.ID,
		"--detached", "1", "--setting-sources", "user,project,local", "--agent", "app", "--media", a.paths.MediaDir(sess.ID),
		"--claude", "/bin/claude", "--model", "opus", "--peers-command", "/bin/ccmux-peers",
	}
	if !slices.Equal(sp.Args, want) || a.asked != 0 {
		t.Fatalf("a person's start line (base asked %d times)\n got %q\nwant %q", a.asked, sp.Args, want)
	}
}

func TestAnAgentWhoseBaseCannotBeReadDoesNotStart(t *testing.T) {
	a := newAgentRig(t)
	a.sup.cfg.Agent = func(store.RunnerSession) (AgentStart, error) { return AgentStart{}, errors.New("no such agent") }
	sess, err := a.sup.Create(agentSession())
	if err != nil {
		t.Fatal(err)
	}
	if a.units.count("start ") != 0 || !strings.Contains(sess.Note, "no such agent") {
		t.Fatalf("no unit starts and the note says why: calls=%v note=%q", a.units.calls, sess.Note)
	}
	a.sup.cfg.Agent = nil
	if _, err := a.sup.cfg.BuildSpec(sess, a.paths); err == nil || !strings.Contains(err.Error(), "no role agents") {
		t.Fatalf("a daemon with no agents says so: %v", err)
	}
}

func TestEveryStartReadsTheBaseAgain(t *testing.T) {
	a := newAgentRig(t)
	var heard []string
	a.sup.Changed = func(s store.RunnerSession) { heard = append(heard, s.Wanted+" "+s.AgentVersion) }
	sess, _ := a.sup.Create(agentSession())
	if err := a.sup.Sleep(sess.ID, false); err != nil {
		t.Fatal(err)
	}
	a.start.Version, a.start.Model = "1.3.0", "haiku"
	if err := a.sup.SetWanted(sess.ID, store.SessionRunning); err != nil {
		t.Fatal(err)
	}
	a.reconcile()
	_, args := argsOf(t, a.rig, sess.ID)
	row, _ := a.st.RunnerSession(sess.ID)
	if !strings.Contains(args, "--model haiku") || row.AgentVersion != "1.3.0" {
		t.Fatalf("a wake starts on the base as it is now: %q, row version %q", args, row.AgentVersion)
	}
	// Whoever follows the row hears the new version: the pane carries it.
	if !slices.Contains(heard, "running 1.3.0") {
		t.Fatalf("the change of version was not passed on: %q", heard)
	}
}

// --- sleep, and the conversation an agent wakes in ---

func TestAFreshSleepWakesInANewConversation(t *testing.T) {
	a := newAgentRig(t)
	sess, _ := a.sup.Create(agentSession())
	unit := a.paths.Unit(sess.ID)
	if err := a.sup.Sleep(sess.ID, true); err != nil {
		t.Fatal(err)
	}
	row, _ := a.st.RunnerSession(sess.ID)
	if row.Wanted != store.SessionAsleep || !row.FreshNext || a.units.count("stop "+unit) != 1 {
		t.Fatalf("asleep, marked fresh, its unit stopped by the sleep itself: %+v %v", row, a.units.calls)
	}
	// A message wakes it, like any session; the start that follows opens
	// the new conversation, with a state file of its own.
	if _, err := a.sup.Send(sess.ID, "the morning run", "bus"); err != nil {
		t.Fatal(err)
	}
	a.reconcile()
	row, _ = a.st.RunnerSession(sess.ID)
	if row.Conversation == "" || row.Conversation == sess.ID || row.FreshNext {
		t.Fatalf("a new conversation, and the mark cleared: %+v", row)
	}
	_, args := argsOf(t, a.rig, sess.ID)
	for _, want := range []string{"--session-auto " + row.Conversation, "--state " + a.paths.StateFile(row.Conversation), "--socket " + a.paths.Socket(sess.ID)} {
		if !strings.Contains(args, want) {
			t.Errorf("the start line lacks %q: %s", want, args)
		}
	}
	// A crash is not a sleep: systemd, or the next pass, starts it again on
	// the same conversation.
	a.units.set(unit, false)
	a.reconcile()
	again, _ := a.st.RunnerSession(sess.ID)
	if again.Conversation != row.Conversation {
		t.Fatalf("a restart that was no sleep resumes: %q then %q", row.Conversation, again.Conversation)
	}
}

func TestASleepThatContinuesKeepsItsConversation(t *testing.T) {
	a := newAgentRig(t)
	n := agentSession()
	n.Conversation = "11111111-2222-3333-4444-555555555555" // the one it had in its pane
	sess, _ := a.sup.Create(n)
	_, args := argsOf(t, a.rig, sess.ID)
	if !strings.Contains(args, "--session-auto "+n.Conversation) {
		t.Fatalf("it picks up the conversation it was given: %s", args)
	}
	if err := a.sup.Sleep(sess.ID, false); err != nil {
		t.Fatal(err)
	}
	a.sup.SetWanted(sess.ID, store.SessionRunning)
	a.reconcile()
	if row, _ := a.st.RunnerSession(sess.ID); row.Conversation != n.Conversation || row.FreshNext {
		t.Fatalf("continue means the same conversation: %+v", row)
	}
}

func TestASleepIsRefusedWhileAMessageWaits(t *testing.T) {
	a := newAgentRig(t)
	sess, _ := a.sup.Create(agentSession())
	if _, err := a.sup.Send(sess.ID, "one more thing", "bus"); err != nil {
		t.Fatal(err)
	}
	// Queued while it ran, so it woke nothing; asleep now, it would wait for
	// whatever woke the session next.
	if err := a.sup.Sleep(sess.ID, true); !errors.Is(err, ErrPromptWaiting) {
		t.Fatalf("a sleep over an unread message: %v", err)
	}
	if row, _ := a.st.RunnerSession(sess.ID); row.Wanted != store.SessionRunning || row.FreshNext || a.units.count("stop ") != 0 {
		t.Fatalf("nothing changed: %+v %v", row, a.units.calls)
	}
}

func TestAStartCanSayWhichConversationItWants(t *testing.T) {
	a := newAgentRig(t)
	sess, _ := a.sup.Create(agentSession())
	// A running session keeps its conversation whatever is asked.
	a.sup.SetFresh(sess.ID, true)
	if row, _ := a.st.RunnerSession(sess.ID); row.FreshNext {
		t.Fatal("a running session is not marked")
	}
	a.sup.Sleep(sess.ID, true)
	a.sup.SetFresh(sess.ID, false) // "continue" on the wake
	a.sup.SetWanted(sess.ID, store.SessionRunning)
	a.reconcile()
	if row, _ := a.st.RunnerSession(sess.ID); row.Conversation != "" {
		t.Fatalf("asked to continue, it resumes: %+v", row)
	}
	// Asleep already: a second sleep only changes the mark.
	a.sup.Sleep(sess.ID, false)
	stops := a.units.count("stop ")
	if err := a.sup.Sleep(sess.ID, true); err != nil {
		t.Fatal(err)
	}
	if row, _ := a.st.RunnerSession(sess.ID); !row.FreshNext || a.units.count("stop ") != stops {
		t.Fatalf("the mark alone: %+v", row)
	}
	a.sup.SetWanted(sess.ID, store.SessionDone)
	if err := a.sup.Sleep(sess.ID, true); !errors.Is(err, ErrSessionDone) {
		t.Fatalf("an ended session does not sleep: %v", err)
	}
}
