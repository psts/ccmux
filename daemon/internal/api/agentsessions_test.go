package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"ccmux.dev/ccmuxd/internal/agent"
	"ccmux.dev/ccmuxd/internal/manager"
	"ccmux.dev/ccmuxd/internal/model"
	"ccmux.dev/ccmuxd/internal/peers"
	"ccmux.dev/ccmuxd/internal/runner"
	"ccmux.dev/ccmuxd/internal/store"
)

// rowSup is a session supervisor that keeps the rows and the panes as the
// real one does, and starts nothing: enough for the agents' flows, which
// read both back.
type rowSup struct {
	*fakeRunners
	st    *store.SQLite
	mgr   *manager.Manager
	made  int
	slept []string // "<id> fresh=<bool>"
	said  []string // "<id>|<sender>|<text>"
	// failCreate, when set, is what the next Create fails with, once.
	failCreate error
	plainSets  []string // "<id>=<wanted>" of every SetWanted
}

func (r *rowSup) sync(id string) {
	if row, _ := r.st.RunnerSession(id); row != nil {
		r.mgr.SyncRunnerPane(*row)
	}
}

func (r *rowSup) Create(n runner.NewSession) (store.RunnerSession, error) {
	if err := r.failCreate; err != nil {
		r.failCreate = nil
		return store.RunnerSession{}, err
	}
	r.made++
	row := store.RunnerSession{ID: fmt.Sprintf("sess-%d", r.made), Name: n.Name, Cwd: n.Cwd, Model: n.Model, Wanted: store.SessionRunning,
		WorkspaceID: n.WorkspaceID, Kind: n.Kind, Title: n.Title, PermissionMode: n.PermissionMode, PaneID: n.PaneID, Conversation: n.Conversation, CreatedAt: int64(r.made)}
	if row.PaneID == "" {
		row.PaneID = "pane-" + row.ID
	}
	if err := r.st.AddRunnerSession(row); err != nil {
		return row, err
	}
	r.created = append(r.created, n)
	r.sync(row.ID)
	return row, nil
}

func (r *rowSup) Sleep(id string, fresh bool) error {
	r.slept = append(r.slept, fmt.Sprintf("%s fresh=%v", id, fresh))
	defer r.sync(id)
	return r.st.SleepRunnerSession(id, fresh, 1)
}

func (r *rowSup) SetFresh(id string, fresh bool) error {
	return r.st.SetRunnerSessionFresh(id, fresh, 1)
}

func (r *rowSup) SetWanted(id, wanted string) error {
	r.plainSets = append(r.plainSets, id+"="+wanted)
	defer r.sync(id)
	return r.st.SetRunnerSessionWanted(id, wanted, 1)
}

func (r *rowSup) Send(id, text, sender string) (string, error) {
	return r.SendAs(id, "p", text, sender)
}

func (r *rowSup) SendAs(id, promptID, text, sender string) (string, error) {
	r.said = append(r.said, id+"|"+sender+"|"+text)
	return promptID, r.SetWanted(id, store.SessionRunning) // a prompt wakes an asleep session
}

func (r *rowSup) Liveness(id string) (string, error) {
	row, err := r.st.RunnerSession(id)
	if err != nil || row == nil {
		return store.SessionDone, err
	}
	return row.Wanted, nil
}

// agentSessionFixture is the window fixture with session runners on, and a
// second base, radar, on the claude harness: idle exit, walls, start fresh.
type agentSessionFixture struct {
	*windowAgentFixture
	sup *rowSup
}

func newAgentSessionFixture(t *testing.T) *agentSessionFixture {
	t.Helper()
	f := &agentSessionFixture{windowAgentFixture: newWindowAgentFixture(t, "sleep 1;:")}
	f.sup = &rowSup{fakeRunners: &fakeRunners{}, st: f.st, mgr: f.srv.mgr}
	f.srv.SetRunners(f.sup, f.st)
	f.srv.mgr.SleepAgentSession = f.srv.SleepAgentSession
	f.srv.ensureSidecar = func() (string, string, error) { return "/s/serve.mjs", "/n/node", nil }
	f.put(t, "/v1/agents/radar", `{"description":"Reads sources.","harness":"claude","model":"sonnet","walls":true,"permissions":{"edit":"allow"},"instructions":"# Role\n"}`, 200)
	return f
}

// post starts base name in the fixture's window with body, and answers the
// status and the pane.
func (f *agentSessionFixture) post(t *testing.T, name, body string) (int, model.Pane) {
	t.Helper()
	resp, err := http.Post(f.base+"/v1/windows/"+f.winID+"/agents/"+name, "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var p model.Pane
	json.NewDecoder(resp.Body).Decode(&p)
	return resp.StatusCode, p
}

func (f *agentSessionFixture) instance(t *testing.T, name string) agentInstance {
	t.Helper()
	win, _, msg := f.srv.windowByID(f.winID)
	if msg != "" {
		t.Fatal(msg)
	}
	d, _, msg := f.srv.agentDefinition(name)
	if msg != "" {
		t.Fatal(msg)
	}
	return f.srv.instanceOf(win, d)
}

func TestAClaudeAgentIsAddedAsASession(t *testing.T) {
	f := newAgentSessionFixture(t)
	code, pane := f.post(t, "radar", `{"prompt":"the morning run","createdBy":"tester"}`)
	if code != 201 || pane.Runtime != model.RuntimeRunner || pane.Agent != "radar" || pane.SessionID != "sess-1" {
		t.Fatalf("add = %d, pane %+v", code, pane)
	}
	made := f.sup.created[0]
	if made.Kind != store.SessionAgent || made.Name != "radar" || made.PermissionMode != "default" || made.Conversation != "" {
		t.Fatalf("its session: %+v", made)
	}
	home := f.srv.mgr.Workspace(made.WorkspaceID)
	if home == nil || home.Agent != "radar" || len(home.Panes) != 0 || home.Status != model.StatusCold || made.Cwd != home.RepoPath {
		t.Fatalf("its home is a workspace with no tmux session: %+v", home)
	}
	if _, err := os.Stat(filepath.Join(home.RepoPath, "AGENTS.md")); err != nil {
		t.Fatalf("its instance folder is bootstrapped: %v", err)
	}
	if !slices.Equal(f.sup.said, []string{"sess-1|ccmux|the morning run"}) {
		t.Fatalf("the prompt is its first message: %q", f.sup.said)
	}
	inst := f.instance(t, "radar")
	if inst.State != "running" || inst.Session != "sess-1" || inst.Pane != pane.ID || inst.Workspace != home.ID {
		t.Fatalf("the window lists it running, with its session: %+v", inst)
	}
	// A second start of a running agent is refused, and makes nothing.
	if code, _ := f.post(t, "radar", `{"prompt":"again"}`); code != 409 || f.sup.made != 1 {
		t.Fatalf("a running agent started again = %d (%d sessions)", code, f.sup.made)
	}
	// The other harness still starts in a pane.
	if code, p := f.start(t, "x-poster", ""); code != 201 || p.StartupCommand == "" {
		t.Fatalf("another harness keeps its pane: %d %+v", code, p)
	}
}

func TestAnAgentSessionSleepsAndWakesByItsBasesRule(t *testing.T) {
	f := newAgentSessionFixture(t)
	f.post(t, "radar", `{"createdBy":"tester"}`)
	req, _ := http.NewRequest("DELETE", f.base+"/v1/windows/"+f.winID+"/agents/radar", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil || resp.StatusCode != 204 {
		t.Fatalf("sleep: %v %v", resp, err)
	}
	// start: fresh is the default: the next start is a new conversation.
	if !slices.Equal(f.sup.slept, []string{"sess-1 fresh=true"}) || f.instance(t, "radar").State != "asleep" {
		t.Fatalf("slept through the door: %q, %+v", f.sup.slept, f.instance(t, "radar"))
	}
	// A wake with a message; this start asks to continue, whatever the base says.
	if code, _ := f.post(t, "radar", `{"prompt":"carry on","resume":true}`); code != 200 {
		t.Fatalf("wake = %d", code)
	}
	row, _ := f.st.RunnerSession("sess-1")
	if row.Wanted != store.SessionRunning || row.FreshNext || f.sup.made != 1 || f.sup.said[len(f.sup.said)-1] != "sess-1|ccmux|carry on" {
		t.Fatalf("the same session, woken by its message, continuing: %+v %q", row, f.sup.said)
	}
	// The thread page's Sleep is the same door.
	f.put(t, "/v1/agents/radar", `{"start":"continue"}`, 200)
	if rec := do(t, f.srv, "POST", "/v1/sessions/sess-1/wanted", `{"wanted":"asleep"}`); rec.Code != 200 {
		t.Fatalf("the page's sleep = %d %s", rec.Code, rec.Body)
	}
	if f.sup.slept[len(f.sup.slept)-1] != "sess-1 fresh=false" {
		t.Fatalf("a base that continues sleeps without the mark: %q", f.sup.slept)
	}
	// A keep-alive wake has no message.
	said := len(f.sup.said)
	if err := f.srv.wakeAgent(row.WorkspaceID, "radar"); err != nil || len(f.sup.said) != said {
		t.Fatalf("a wake with nothing to say: %v %q", err, f.sup.said)
	}
	if row, _ = f.st.RunnerSession("sess-1"); row.Wanted != store.SessionRunning {
		t.Fatalf("it runs again: %+v", row)
	}
	// An agent's model is its base's.
	if rec := do(t, f.srv, "POST", "/v1/sessions/sess-1/model", `{"model":"opus"}`); rec.Code != 409 {
		t.Fatalf("a model set on an agent's session = %d", rec.Code)
	}
}

func TestTheCapCountsAgentSessions(t *testing.T) {
	f := newAgentSessionFixture(t)
	f.put(t, "/v1/agents/scout", `{"description":"Reads.","harness":"claude","instructions":"# Role\n"}`, 200)
	f.srv.agentsMax, f.srv.mgr.AgentsMax = 1, 1
	if code, _ := f.post(t, "radar", `{}`); code != 201 {
		t.Fatalf("the first agent = %d", code)
	}
	if code, _ := f.post(t, "scout", `{}`); code != 409 || f.sup.made != 1 {
		t.Fatalf("a second agent over the cap = %d (%d sessions)", code, f.sup.made)
	}
	// Asleep, radar frees the slot; a message for it while scout holds the
	// slot waits in the mailbox, and wakes nothing.
	f.srv.SleepAgentSession("sess-1")
	if code, _ := f.post(t, "scout", `{}`); code != 201 {
		t.Fatalf("scout once radar sleeps = %d", code)
	}
	radar, _ := f.st.RunnerSession("sess-1")
	said := len(f.sup.said)
	if err := f.srv.runnerInbox(radar.PaneID, "p1", "hello", "hq"); err == nil || len(f.sup.said) != said {
		t.Fatalf("a message over the cap must wait: %v %q", err, f.sup.said)
	}
	for _, wake := range [][2]string{{"wanted", `{"wanted":"running"}`}, {"prompt", `{"text":"wake up"}`}, {"restart", `{}`}} {
		if rec := do(t, f.srv, "POST", "/v1/sessions/sess-1/"+wake[0], wake[1]); rec.Code != 409 {
			t.Fatalf("a %s from the page over the cap = %d", wake[0], rec.Code)
		}
	}
	if len(f.sup.said) != said {
		t.Fatalf("nothing reached it: %q", f.sup.said)
	}
	f.srv.SleepAgentSession("sess-2")
	if err := f.srv.runnerInbox(radar.PaneID, "p1", "hello", "hq"); err != nil || f.sup.said[len(f.sup.said)-1] != "sess-1|hq|hello" {
		t.Fatalf("handed over once a slot is free: %v %q", err, f.sup.said)
	}
	// A message for a running agent is never held, whatever the cap says.
	if err := f.srv.runnerInbox(radar.PaneID, "p2", "and this", "hq"); err != nil {
		t.Fatalf("a running agent's message: %v", err)
	}
	if err := f.srv.runnerInbox("no-such-pane", "p3", "x", "hq"); err != peers.ErrInboxClosed {
		t.Fatalf("a pane that is gone: %v", err)
	}
}

func TestASchedulePromptsAnAgentSession(t *testing.T) {
	f := newAgentSessionFixture(t)
	f.srv.SetSchedules(f.st, 6*time.Hour)
	win, _, _ := f.srv.windowByID(f.winID)
	d, _, _ := f.srv.agentDefinition("radar")
	// Not in the window yet: the schedule starts it.
	if action, _ := f.srv.decideFire(win, d); action != fireStart {
		t.Fatalf("an absent agent: %v", action)
	}
	_, pane := f.post(t, "radar", `{}`)
	win, _, _ = f.srv.windowByID(f.winID) // its home is a member now
	action, target := f.srv.decideFire(win, d)
	if action != firePush || target != pane.ID {
		t.Fatalf("a running, idle agent session takes the prompt: %v %q", action, target)
	}
	now := time.Now()
	id, _ := f.st.AddAgentSchedule(store.AgentSchedule{WindowID: f.winID, Agent: "radar", Cron: "@hourly", Prompt: "read the feeds", NextRunAt: now.Add(-time.Second).UnixMilli()})
	f.srv.fireDueSchedules()
	last := f.sup.said[len(f.sup.said)-1]
	if !strings.HasPrefix(last, "sess-1|"+scheduleSource+"|[ccmux schedule #") || !strings.HasSuffix(last, "read the feeds") {
		t.Fatalf("the prompt went to its session's inbox: %q", f.sup.said)
	}
	if sc, _ := f.st.AgentSchedule(id); sc == nil || sc.LastRunAt == 0 {
		t.Fatalf("the run is recorded: %+v", sc)
	}
	// Busy: wait for the next tick.
	f.srv.mgr.ApplyAgentSignal(pane.ID, model.AttentionRunning, model.ReasonNone)
	if action, _ := f.srv.decideFire(win, d); action != fireWait {
		t.Fatalf("a busy agent: %v", action)
	}
	// Asleep: the start flow wakes it with the prompt.
	f.srv.mgr.ApplyAgentSignal(pane.ID, model.AttentionDone, model.ReasonFinished)
	f.srv.SleepAgentSession("sess-1")
	if action, _ := f.srv.decideFire(win, d); action != fireStart {
		t.Fatalf("an asleep agent: %v", action)
	}
	if ok, err := f.srv.deliverSchedule(fireStart, "", win, "radar", "the evening run"); !ok || err != nil {
		t.Fatalf("delivered by waking it: %v %v", ok, err)
	}
	if f.sup.said[len(f.sup.said)-1] != "sess-1|ccmux|the evening run" || f.sup.made != 1 {
		t.Fatalf("woken with the prompt, the same session: %q", f.sup.said)
	}
	// Its own schedule tool finds it by its pane.
	if _, name, status, msg := f.srv.instanceOfPane(pane.ID); name != "radar" || msg != "" {
		t.Fatalf("the schedule tool on an agent session's pane: %q %d %s", name, status, msg)
	}
}

func TestAgentStartIsThePanesLaunchFromTheBaseAsItIs(t *testing.T) {
	f := newAgentSessionFixture(t)
	_, pane := f.post(t, "radar", `{}`)
	row, _ := f.st.RunnerSession("sess-1")
	win, _, _ := f.srv.windowByID(f.winID)
	shared := f.srv.agents.ExistingSharedDir(win.ID, win.Name)
	os.WriteFile(filepath.Join(shared, agent.EnvFile), []byte("KB_URL=http://kb\nTOKEN=shared\nPATH=/evil\n"), 0o600)
	os.WriteFile(filepath.Join(row.Cwd, agent.EnvFile), []byte("TOKEN=mine\n"), 0o600)

	start, err := f.srv.AgentStart(*row)
	if err != nil {
		t.Fatal(err)
	}
	base := f.srv.agents.Dir("radar")
	args := strings.Join(start.Args, " ")
	for _, want := range []string{
		"--plugin-dir " + base, "--system-prompt-file " + filepath.Join(base, "AGENTS.md"), "--mcp-file " + filepath.Join(base, "mcp.json"),
		"--add-dir /tmp", "--add-dir " + shared, "--write-root " + row.Cwd, "--write-root " + filepath.Join(shared, "radar"), "--write-inside allow",
	} {
		if !strings.Contains(args, want) {
			t.Errorf("its flags lack %q: %s", want, args)
		}
	}
	for _, never := range []string{"--port", "--peers-command", "--model", "--session"} {
		if strings.Contains(args, never) {
			t.Errorf("%q is its runner's to say, not its base's: %s", never, args)
		}
	}
	if start.Model != "sonnet" || start.Version == "" {
		t.Errorf("its base's model and version: %+v", start)
	}
	// The window's .env, then its own on top; a variable a .env may not set
	// is not set here either.
	if start.Env["KB_URL"] != "http://kb" || start.Env["TOKEN"] != "mine" || start.Env["PATH"] != "" {
		t.Errorf("its .env files: %+v", start.Env)
	}
	if !claudeProjectTrusted(t, f.srv.claudeConfig, row.Cwd) {
		t.Error("every start marks its folder trusted: a headless session is asked nothing")
	}
	// The base moved on while it slept: the next start is the new base.
	f.put(t, "/v1/agents/radar", `{"model":"haiku","addDirs":["/srv/feeds"]}`, 200)
	start, err = f.srv.AgentStart(*row)
	if err != nil || start.Model != "haiku" || !strings.Contains(strings.Join(start.Args, " "), "--add-dir /srv/feeds") {
		t.Fatalf("a start reads the base again: %+v %v", start, err)
	}
	// A base moved to a harness that starts in a terminal fails the start,
	// with what to do.
	f.put(t, "/v1/agents/radar", `{"harness":"noop","walls":false}`, 200)
	if _, err := f.srv.AgentStart(*row); err == nil || !strings.Contains(err.Error(), "remove this agent") {
		t.Fatalf("a base on another harness: %v", err)
	}
	_ = pane
}

// An agent that ran in a tmux pane until now moves at its next start: its
// pane's id goes to its session, and a base that continues picks up the
// conversation it had.
func TestAnAgentInATmuxPaneMovesAtItsNextStart(t *testing.T) {
	f := newAgentSessionFixture(t)
	// Its home as it was: started by a daemon with no session runners, so
	// in a tmux pane. The launch line's node is not there, so the pane is
	// back at its shell at once: an asleep agent in a live tmux session.
	sup, rows := f.srv.runners, f.srv.runnerRows
	f.srv.runners, f.srv.runnerRows = nil, nil
	code, old := f.start(t, "radar", "")
	if code != 201 || old.StartupCommand == "" {
		t.Fatalf("the pane start = %d %+v", code, old)
	}
	f.srv.runners, f.srv.runnerRows = sup, rows
	// Asleep, and past the moment in which a start typed into the pane
	// still counts as running there.
	for end := time.Now().Add(15 * time.Second); f.srv.mgr.TmuxAgentUp(old.WorkspaceID) || !f.srv.mgr.PaneAtShell(old.ID); time.Sleep(200 * time.Millisecond) {
		if time.Now().After(end) {
			t.Fatal("the pane never read as asleep")
		}
	}
	home := f.srv.mgr.Workspace(old.WorkspaceID)
	if inst := f.instance(t, "radar"); inst.State != "asleep" || inst.Pane != old.ID || inst.Session != "" || home.Status != model.StatusLive {
		t.Fatalf("before: asleep in its pane, its tmux session open: %+v", inst)
	}
	f.put(t, "/v1/agents/radar", `{"start":"continue"}`, 200)
	f.srv.offlineSessions = func(_ context.Context, _ string, d string) ([]agent.OpencodeSession, error) {
		return []agent.OpencodeSession{{ID: "conv-from-the-pane", Directory: d}}, nil
	}
	// A session that could not be made leaves the home as it was: its tmux
	// session open, its pane, and so its id, still there for the next start.
	f.sup.failCreate = fmt.Errorf("database is locked")
	if code, _ := f.post(t, "radar", `{"prompt":"back to work"}`); code != 500 {
		t.Fatalf("a start whose session could not be made = %d", code)
	}
	if ws := f.srv.mgr.Workspace(home.ID); len(ws.Panes) != 1 || ws.Panes[0].ID != old.ID || ws.Status != model.StatusLive || f.sup.made != 0 {
		t.Fatalf("nothing was taken off tmux for a session that does not exist: %+v", ws)
	}
	code, pane := f.post(t, "radar", `{"prompt":"back to work"}`)
	if code != 200 || pane.ID != old.ID || pane.Runtime != model.RuntimeRunner {
		t.Fatalf("its next start = %d, pane %+v", code, pane)
	}
	made := f.sup.created[0]
	if made.PaneID != old.ID || made.Conversation != "conv-from-the-pane" || made.WorkspaceID != home.ID {
		t.Fatalf("its session takes the pane's id and its conversation: %+v", made)
	}
	if ws := f.srv.mgr.Workspace(home.ID); len(ws.Panes) != 0 || ws.Status != model.StatusCold {
		t.Fatalf("its tmux session is closed, and no pane is left with the old launch line: %+v", ws)
	}
	// Nothing in tmux is left for the next pass to adopt again.
	f.srv.mgr.Reconcile()
	if ws := f.srv.mgr.Workspace(home.ID); ws.Status != model.StatusCold {
		t.Fatalf("the old tmux session came back: %+v", ws)
	}
	if inst := f.instance(t, "radar"); inst.State != "running" || inst.Session != "sess-1" {
		t.Fatalf("after: running as a session: %+v", inst)
	}
	// A lens that had its old chat open is told where it went, and the old
	// chat routes say so too instead of waiting on a port that is gone.
	c := newChatConn(f.srv, &pane, func() {})
	if !c.movedToSession() {
		t.Fatal("the chat on its old pane must see the move")
	}
	said, moved := <-c.out, <-c.out
	if said.T != "error" || !strings.Contains(said.Error, "/threads.html?s=sess-1") || moved.T != "moved" || moved.URL != "/threads.html?s=sess-1" {
		t.Fatalf("frames: %+v %+v", said, moved)
	}
	for _, path := range []string{"/agent", "/agent/reply"} {
		method := map[string]string{"/agent": "GET", "/agent/reply": "POST"}[path]
		if rec := do(t, f.srv, method, "/v1/panes/"+pane.ID+path, `{}`); rec.Code != 409 || !strings.Contains(rec.Body.String(), "/threads.html?s=sess-1") {
			t.Fatalf("%s %s on a session's pane = %d %s", method, path, rec.Code, rec.Body)
		}
	}
	// A pane that is still a tmux agent's has not moved.
	_, tmuxPane := f.start(t, "x-poster", "")
	if newChatConn(f.srv, &model.Pane{ID: tmuxPane.ID, Agent: "x-poster"}, func() {}).movedToSession() {
		t.Fatal("a tmux agent's chat stays where it is")
	}
}

// A start that cannot work is said to whoever asked, before anything is
// made: a session's own failed start is quiet (tried again, then parked).
func TestAnAgentThatCannotStartIsRefusedBeforeItsSessionIsMade(t *testing.T) {
	f := newAgentSessionFixture(t)
	if err := os.WriteFile(f.srv.claudeConfig, []byte("{oops"), 0o600); err != nil {
		t.Fatal(err)
	}
	before := len(f.srv.mgr.List())
	code, _ := f.post(t, "radar", `{"prompt":"go"}`)
	if code != 503 || f.sup.made != 0 || len(f.srv.mgr.List()) != before {
		t.Fatalf("a Claude config that does not parse: %d, %d sessions, %d workspaces (was %d)", code, f.sup.made, len(f.srv.mgr.List()), before)
	}
	if err := f.srv.startAgentForPeer("Chart Labs", "radar", "birth"); err == nil || !strings.Contains(err.Error(), "untouched") {
		t.Fatalf("the bus hears why, to tell the sender: %v", err)
	}
	os.Remove(f.srv.claudeConfig)
	// A host that cannot land the sidecar: the same, with its reason.
	f.srv.ensureSidecar = func() (string, string, error) { return "", "", fmt.Errorf("needs node") }
	if code, _ := f.post(t, "radar", `{}`); code != 503 || f.sup.made != 0 {
		t.Fatalf("no sidecar: %d", code)
	}
}

// The bus starts a session with the request alone: it reaches the inbox
// when the session's peer is on the bus, so the birth prompt a pane needs
// would only be a second turn.
func TestTheBusStartsASessionWithoutABirthPrompt(t *testing.T) {
	f := newAgentSessionFixture(t)
	win, _, _ := f.srv.windowByID(f.winID)
	if got := f.srv.birthPromptFor(win, "radar", "you were just started"); got != "" {
		t.Fatalf("a claude agent, not here yet: %q", got)
	}
	if got := f.srv.birthPromptFor(win, "x-poster", "you were just started"); got != "you were just started" {
		t.Fatalf("an agent that starts in a pane keeps it: %q", got)
	}
	if err := f.srv.startAgentForPeer("Chart Labs", "radar", "you were just started"); err != nil {
		t.Fatal(err)
	}
	if f.sup.made != 1 || len(f.sup.said) != 0 {
		t.Fatalf("started, with nothing said by the daemon: %d sessions, %q", f.sup.made, f.sup.said)
	}
	// Running already: not an error for the bus, which delivers to it.
	if err := f.srv.startAgentForPeer("Chart Labs", "radar", "again"); err != nil {
		t.Fatalf("a running agent is not a failed start: %v", err)
	}
}

// unreadableRows is the registry on a day a session's row cannot be read.
type unreadableRows struct {
	runnerRows
	broken map[string]bool
}

func (u unreadableRows) RunnerSession(id string) (*store.RunnerSession, error) {
	if u.broken[id] {
		return nil, fmt.Errorf("database is locked")
	}
	return u.runnerRows.RunnerSession(id)
}

// A row that cannot be read is never taken for "not an agent": the plain
// route would put the agent to sleep past its door, or wake it past the cap.
func TestAnUnreadableRowNeverSendsAnAgentPastItsRules(t *testing.T) {
	f := newAgentSessionFixture(t)
	_, pane := f.post(t, "radar", `{}`)
	// A thread of the repo beside it, which the agents' rules do not touch.
	thread, err := f.sup.Create(runner.NewSession{Cwd: "/tmp", WorkspaceID: f.ws.ID})
	if err != nil {
		t.Fatal(err)
	}
	f.srv.runnerRows = unreadableRows{runnerRows: f.st, broken: map[string]bool{"sess-1": true}}
	sets := len(f.sup.plainSets)
	for _, wanted := range []string{"asleep", "running"} {
		if rec := do(t, f.srv, "POST", "/v1/sessions/sess-1/wanted", `{"wanted":"`+wanted+`"}`); rec.Code != 500 {
			t.Fatalf("wanted %s on a row that cannot be read = %d %s", wanted, rec.Code, rec.Body)
		}
	}
	if len(f.sup.plainSets) != sets || len(f.sup.slept) != 0 {
		t.Fatalf("nothing was changed: sets %q slept %q", f.sup.plainSets[sets:], f.sup.slept)
	}
	if err := f.srv.runnerInbox(pane.ID, "p1", "hello", "hq"); err == nil || len(f.sup.said) != 0 {
		t.Fatalf("a message waits while its session cannot be read: %v %q", err, f.sup.said)
	}
	// The thread's row reads fine, and its sleep is the plain one.
	if rec := do(t, f.srv, "POST", "/v1/sessions/"+thread.ID+"/wanted", `{"wanted":"asleep"}`); rec.Code != 200 {
		t.Fatalf("a thread's sleep = %d %s", rec.Code, rec.Body)
	}
	if got := f.sup.plainSets[len(f.sup.plainSets)-1]; got != thread.ID+"=asleep" || len(f.sup.slept) != 0 {
		t.Fatalf("a thread sleeps the plain way, not through the agents' door: %q %q", got, f.sup.slept)
	}
}

// The agent editor's "restart instances": a running agent session is put to
// sleep through the door and started again, the same session, on the base
// as it now is. An asleep one is left asleep.
func TestRestartInstancesRestartsAnAgentSession(t *testing.T) {
	f := newAgentSessionFixture(t)
	f.put(t, "/v1/agents/scout", `{"description":"Reads.","harness":"claude","instructions":"# Role\n"}`, 200)
	f.post(t, "radar", `{}`)
	f.post(t, "scout", `{}`)
	f.srv.SleepAgentSession("sess-2") // scout sleeps: a restart of radar's instances must not wake it
	sleeps := len(f.sup.slept)
	if rec := do(t, f.srv, "POST", "/v1/agents/radar/instances/restart", ``); rec.Code != 202 || !strings.Contains(rec.Body.String(), `"restarting":1`) {
		t.Fatalf("restart = %d %s", rec.Code, rec.Body)
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		row, _ := f.st.RunnerSession("sess-1")
		if len(f.sup.slept) > sleeps && row.Wanted == store.SessionRunning {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("never slept and started again: slept %q, row %+v", f.sup.slept, row)
		}
		time.Sleep(50 * time.Millisecond)
	}
	if got := f.sup.slept[sleeps:]; len(got) != 1 || got[0] != "sess-1 fresh=true" {
		t.Fatalf("slept once, through the door, by its base's rule: %q", got)
	}
	if f.sup.made != 2 {
		t.Fatalf("the same session, not a new one: %d sessions", f.sup.made)
	}
	if scout, _ := f.st.RunnerSession("sess-2"); scout.Wanted != store.SessionAsleep {
		t.Fatalf("another agent's asleep session was woken: %+v", scout)
	}
	if rec := do(t, f.srv, "POST", "/v1/agents/scout/instances/restart", ``); rec.Code != 202 || !strings.Contains(rec.Body.String(), `"restarting":0`) {
		t.Fatalf("an asleep instance is not restarted: %d %s", rec.Code, rec.Body)
	}
}
