package main

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"ccmux.dev/ccmuxd/internal/buscontext"
)

// The rendering lives in internal/buscontext and is tested there; this pins
// that the shim carries both lists and says nothing outside a window.
func TestWindowParagraphAndSectionWithoutADaemon(t *testing.T) {
	a := &app{}
	if a.windowLists(a.sessions(), instructionsCap()) != "" || a.windowSection() != "" {
		t.Fatal("no daemon: nothing appended")
	}
	if !strings.Contains(strings.ToLower(serverInstructions), "peer") {
		t.Fatal("frozen instructions missing")
	}
}

// The daemon says who is reading; the shim must hand that on to the text.
// A role agent gets the repo folders and may read there; a repo session
// gets neither, sees itself marked, and is told to hand the work over.
func TestWindowParagraphFollowsTheReader(t *testing.T) {
	reader := `{"agent":true}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/peers/sessions":
			fmt.Fprintf(w, `{"sessions":[{"name":"backend","repoPath":"/r/backend","owns":"API","status":"live"},`+
				`{"name":"frontend","repoPath":"/r/frontend","status":"live"}],"shared":"","reader":%s}`, reader)
		default:
			fmt.Fprint(w, `[]`)
		}
	}))
	defer srv.Close()
	a := &app{daemon: newDaemonClient(srv.URL, "t"), id: "p1"}

	for _, text := range []string{a.windowLists(a.sessions(), instructionsCap()), a.windowSection()} {
		if !strings.Contains(text, "/r/backend") || !strings.Contains(text, "owns API") {
			t.Fatalf("a role agent gets the folders and the owns lines:\n%s", text)
		}
	}
	if !strings.Contains(a.windowLists(a.sessions(), instructionsCap()), "read there") {
		t.Fatal("a role agent may read the repos it reaches")
	}

	reader = `{"agent":false,"self":"frontend"}`
	for _, text := range []string{a.windowLists(a.sessions(), instructionsCap()), a.windowSection()} {
		if strings.Contains(text, "/r/") || !strings.Contains(text, "frontend (you)") || !strings.Contains(text, "delegate") {
			t.Fatalf("a repo session gets no folders, itself marked, and the hand-over rule:\n%s", text)
		}
	}
}

// sessionsServer answers /v1/peers/sessions with the given lists, read at
// each request so a test can change them between calls.
func sessionsServer(t *testing.T, reader, sessions *string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/peers/sessions" {
			fmt.Fprintf(w, `{"sessions":%s,"shared":"/shared/proj","reader":%s}`, *sessions, *reader)
			return
		}
		fmt.Fprint(w, `[]`)
	}))
	t.Cleanup(srv.Close)
	return srv
}

const buildOrFile = "Build it now, or file it as a task?"

// The build-or-file rule goes only to a session that can file a task: a
// repo session with a pane, in a window. A role agent, a session outside a
// window (no sessions listed) and a Claude with no ccmux pane get nothing,
// since filing would fail for each of them.
func TestTaskQueueRuleOnlyWhereFilingWorks(t *testing.T) {
	reader, sessions := `{"agent":false,"self":"app"}`, `[{"name":"app","repoPath":"/r/app","status":"live"}]`
	srv := sessionsServer(t, &reader, &sessions)
	a := &app{daemon: newDaemonClient(srv.URL, "t"), id: "p1", paneID: "pane-1"}
	if !strings.Contains(a.instructions(), buildOrFile) {
		t.Fatal("a repo session in a window was not given the build-or-file rule")
	}
	if !strings.Contains(a.instructions(), "still in plan mode") {
		t.Fatal("the rule lost its plan-mode route")
	}

	reader = `{"agent":true}`
	if strings.Contains(a.instructions(), buildOrFile) {
		t.Fatal("a role agent was told to file tasks")
	}
	reader, sessions = `{"agent":false,"self":"app"}`, `[]`
	if strings.Contains(a.instructions(), buildOrFile) {
		t.Fatal("a session in no window was told to file tasks")
	}
	sessions = `[{"name":"app","repoPath":"/r/app","status":"live"}]`
	a.paneID = ""
	if strings.Contains(a.instructions(), buildOrFile) {
		t.Fatal("a Claude with no ccmux pane was told to file tasks")
	}
	if strings.Contains(serverInstructions, buildOrFile) {
		t.Fatal("the rule is back in the fixed text every session gets")
	}
}

// The to-do rule is what makes sessions add to their repo's list unasked.
// It goes to every session with a repo's list behind it: a ccmux pane's
// own, in a window or not. A role agent has no repo and a Claude outside
// ccmux has no pane to be known by, so neither is told to use a tool that
// would refuse them.
func TestTodoRuleOnlyWhereThereIsAList(t *testing.T) {
	const addUnasked = "add it with the todo tool, unasked"
	reader, sessions := `{"agent":false,"self":"app"}`, `[{"name":"app","repoPath":"/r/app","status":"live"}]`
	srv := sessionsServer(t, &reader, &sessions)
	a := &app{daemon: newDaemonClient(srv.URL, "t"), id: "p1", paneID: "pane-1"}
	got := a.instructions()
	if !strings.Contains(got, addUnasked) {
		t.Fatal("a repo session was not told to add to its to-do list")
	}
	for _, want := range []string{"Also found", "high if", "medium if", "else low", "List first", "mark an item done"} {
		if !strings.Contains(todoRule, want) {
			t.Errorf("the rule lost %q", want)
		}
	}
	sessions = `[]`
	if got := a.instructions(); !strings.Contains(got, addUnasked) || strings.Contains(got, buildOrFile) {
		t.Fatal("a repo session in no window has a list but no task queue: the to-do rule alone")
	}
	reader, sessions = `{"agent":true}`, `[{"name":"app","repoPath":"/r/app","status":"live"}]`
	if strings.Contains(a.instructions(), addUnasked) {
		t.Fatal("a role agent, which has no repo, was told to add to-dos")
	}
	reader = `{"agent":false,"self":"app"}`
	a.paneID = ""
	if strings.Contains(a.instructions(), addUnasked) {
		t.Fatal("a Claude with no ccmux pane was told to add to-dos")
	}
	if strings.Contains(serverInstructions, addUnasked) {
		t.Fatal("the rule is in the fixed text every session gets")
	}
}

// Claude Code keeps only the first instructionsCap characters. The fixed
// rules and the task-queue rule must end inside that, and stay ahead of the
// window's lists however long those get.
func TestInstructionsKeepTheRulesInsideTheCap(t *testing.T) {
	t.Setenv(buscontext.CapEnv, "")
	var list []string
	for i := 0; i < 12; i++ {
		list = append(list, fmt.Sprintf(`{"name":"repo-%d","repoPath":"/r/repo-%d","owns":"a long description of what this repo owns and serves","status":"live"}`, i, i))
	}
	reader, sessions := `{"agent":false,"self":"repo-0"}`, "["+strings.Join(list, ",")+"]"
	srv := sessionsServer(t, &reader, &sessions)
	a := &app{daemon: newDaemonClient(srv.URL, "t"), id: "p1", paneID: "pane-1"}
	got := a.instructions()
	end := strings.Index(got, taskQueueRule) + len(taskQueueRule)
	if strings.Index(got, taskQueueRule) != len(serverInstructions) || end > buscontext.DefaultCap {
		t.Fatalf("the task-queue rule ends at %d (cap %d), right after the fixed rules: %v", end, buscontext.DefaultCap, strings.Index(got, taskQueueRule) == len(serverInstructions))
	}
	if strings.Index(got, todoRule) != end {
		t.Fatalf("the to-do rule starts at %d, want right after the task-queue rule at %d", strings.Index(got, todoRule), end)
	}
	if end += len(todoRule); end > buscontext.DefaultCap {
		t.Fatalf("the to-do rule ends at %d, past the cap of %d", end, buscontext.DefaultCap)
	}
	if !strings.Contains(got, "PROJECT SESSIONS IN THIS WINDOW") || strings.Index(got, "PROJECT SESSIONS") < end {
		t.Fatal("the window's lists must come after the rules")
	}
}

// bigWindowApp is a session in a window like the ones in use: eight repos
// that say what they own, a shared folder, eight agents with a sentence
// each. Its whole paragraph is some 3,500 characters; reader is read at each
// request.
func bigWindowApp(t *testing.T, reader *string) *app {
	t.Helper()
	var sessions, agents []string
	for i := 0; i < 8; i++ {
		sessions = append(sessions, fmt.Sprintf(`{"name":"repo-%d","repoPath":"/r/repo-%d","owns":"a long description of what this repo owns and serves","status":"cold"}`, i, i))
		agents = append(agents, fmt.Sprintf(`{"name":"agent-%d","description":"Reads the project's outside sources each weekday morning and files what it finds.","state":"absent"}`, i))
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/peers/sessions":
			fmt.Fprintf(w, `{"sessions":[%s],"shared":"/shared/proj","reader":%s}`, strings.Join(sessions, ","), *reader)
		case "/v1/peers/agents":
			fmt.Fprintf(w, `[%s]`, strings.Join(agents, ","))
		default:
			fmt.Fprint(w, `[]`)
		}
	}))
	t.Cleanup(srv.Close)
	return &app{daemon: newDaemonClient(srv.URL, "t"), id: "p1", paneID: "pane-1"}
}

// agentDoes is in the window's paragraph only while it is whole: the
// shortened forms give up what each agent does first.
const agentDoes = "files what it finds"

// A session started outside ccmux has Claude Code's own cap. The window's
// lists get what the rules leave of it, and are made shorter to fit instead
// of being cut by Claude Code: a cut loses the end of the lists (the shared
// folder and every agent, in most windows) without a word. Shortened, every
// session and the shared folder are still there, the agents are at least
// counted, and the text says where the rest is.
func TestInstructionsFitTheWindowsListsInsideTheCap(t *testing.T) {
	t.Setenv(buscontext.CapEnv, "")
	reader := `{"agent":false,"self":"repo-0"}`
	a := bigWindowApp(t, &reader)

	check := func(who, rules string) {
		t.Helper()
		got := a.instructions()
		if len(got) > buscontext.DefaultCap || !strings.HasPrefix(got, rules) || !strings.HasSuffix(got, "\n") {
			t.Fatalf("%s: %d chars for a cap of %d, the rules first and a whole last line:\n%s", who, len(got), buscontext.DefaultCap, got)
		}
		lists := got[len(rules):]
		if strings.Contains(lists, agentDoes) {
			t.Fatalf("%s: this window's long form cannot fit, yet it was sent", who)
		}
		for _, want := range []string{"repo-0", "repo-7", "SHARED FOLDER of this window: /shared/proj.", "AGENTS ON THIS BUS", "list_peers"} {
			if !strings.Contains(lists, want) {
				t.Fatalf("%s: the shortened lists lost %q:\n%s", who, want, lists)
			}
		}
	}
	check("a repo session", serverInstructions+taskQueueRule+todoRule)
	// A role agent has no task-queue rule and no to-do rule, so more room:
	// it keeps the folders it reads the code at.
	reader = `{"agent":true}`
	check("a role agent", serverInstructions)
	if got := a.instructions(); !strings.Contains(got, "/r/repo-7") || !strings.Contains(got, "agent-7") {
		t.Fatalf("a role agent's room holds the folders and the agents' names:\n%s", got[len(serverInstructions):])
	}
}

// ccmux raises Claude Code's cap in every pane it starts, and Claude Code
// starts the shim with that same environment. The shim must use the room it
// was given: with the raised cap the same window's lists go out whole, and a
// value Claude Code would not take as a length leaves its default in force.
func TestInstructionsUseTheCapClaudeCodeWasGiven(t *testing.T) {
	reader := `{"agent":false,"self":"repo-0"}`
	a := bigWindowApp(t, &reader)

	t.Setenv(buscontext.CapEnv, buscontext.RaisedCapValue)
	if instructionsCap() != buscontext.RaisedCap {
		t.Fatalf("cap = %d with %s=%s", instructionsCap(), buscontext.CapEnv, buscontext.RaisedCapValue)
	}
	got := a.instructions()
	if len(got) > buscontext.RaisedCap || strings.Count(got, agentDoes) != 8 || !strings.Contains(got, "Its .env is loaded") {
		t.Fatalf("with the raised cap the whole paragraph fits (%d chars), yet it was shortened:\n%s", len(got), got[len(serverInstructions)+len(taskQueueRule)+len(todoRule):])
	}
	if !strings.HasPrefix(got, serverInstructions+taskQueueRule+todoRule) {
		t.Fatal("with the raised cap the rules still come first, the to-do rule last of them")
	}

	t.Setenv(buscontext.CapEnv, "plenty")
	if got := a.instructions(); instructionsCap() != buscontext.DefaultCap || len(got) > buscontext.DefaultCap {
		t.Fatalf("a value that is no length: cap %d, %d chars sent", instructionsCap(), len(got))
	}
}

// The handshake waits briefly for the first registration: answered before
// it, the daemon could not say which window this session is in, and the
// session would run without its window's parts for good.
func TestInstructionsWaitForTheFirstRegistration(t *testing.T) {
	reader, sessions := `{"agent":false,"self":"app"}`, `[{"name":"app","repoPath":"/r/app","status":"live"}]`
	srv := sessionsServer(t, &reader, &sessions)
	defer func(was func() bool) { ownsPaneBus = was }(ownsPaneBus)

	// A process that does not own the pane (a subagent, a warm spare) never
	// registers, so it must not wait at all.
	ownsPaneBus = func() bool { return false }
	spare := &app{daemon: newDaemonClient(srv.URL, "t"), paneID: "pane-1", registered: make(chan struct{})}
	start := time.Now()
	spare.instructions()
	if waited := time.Since(start); waited > registerWait/2 {
		t.Fatalf("a non-owner waited %s for a registration that never comes", waited)
	}

	ownsPaneBus = func() bool { return true }
	a := &app{daemon: newDaemonClient(srv.URL, "t"), paneID: "pane-1", registered: make(chan struct{})}
	go func() {
		time.Sleep(50 * time.Millisecond)
		a.mu.Lock()
		a.id = "p1"
		a.mu.Unlock()
		a.markRegistered()
	}()
	if !strings.Contains(a.instructions(), buildOrFile) {
		t.Fatal("a registration landing just after the handshake left the window's parts out")
	}
}

// Pushes wait for Claude Code's notifications/initialized: one sent before
// it is dropped by the client yet counted as shown and acked, so a queued
// delegation would be lost. The notice, and only the notice, releases them.
func TestPushesWaitForTheHandshake(t *testing.T) {
	a := newApp()
	a.installHandlers()
	released := make(chan struct{})
	go func() { a.waitClientReady(); close(released) }()
	select {
	case <-released:
		t.Fatal("pushes were released before the handshake finished")
	case <-time.After(50 * time.Millisecond):
	}
	a.mcp.onNotify["notifications/initialized"](nil)
	select {
	case <-released:
	case <-time.After(time.Second):
		t.Fatal("notifications/initialized did not release the pushes")
	}
}

// The push loop itself does not dial until the handshake is done, and then
// does: one push sent earlier is dropped by Claude Code yet acked.
func TestPushLoopDialsOnlyAfterTheHandshake(t *testing.T) {
	var dials atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/peers/ws":
			dials.Add(1)
			http.Error(w, "no", http.StatusBadRequest)
		case "/v1/peers/register":
			fmt.Fprint(w, `{"peer_id":"p1","name":"app","group":"g"}`)
		default:
			fmt.Fprint(w, `[]`)
		}
	}))
	defer srv.Close()
	var owner atomic.Bool
	owner.Store(true)
	defer func(was func() bool) { ownsPaneBus = was }(ownsPaneBus)
	ownsPaneBus = owner.Load
	a := newApp()
	a.daemon, a.id = newDaemonClient(srv.URL, "t"), "p1"
	a.installHandlers()
	done := make(chan struct{})
	go func() { a.runPushLoop(); close(done) }()
	time.Sleep(100 * time.Millisecond)
	if n := dials.Load(); n != 0 {
		t.Fatalf("the push loop dialled %d times before the handshake finished", n)
	}
	a.mcp.onNotify["notifications/initialized"](nil)
	deadline := time.Now().Add(2 * time.Second)
	for dials.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	owner.Store(false)
	if dials.Load() == 0 {
		t.Fatal("the push loop never dialled after the handshake")
	}
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("the push loop did not stop once it lost the pane")
	}
}

// A pane's own session whose registration never comes still gets its
// handshake answered, within registerWait and with the fixed rules: a wait
// without its timeout would hang initialize, and the session would have no
// bus at all.
func TestInstructionsDoNotHangWithoutARegistration(t *testing.T) {
	defer func(was func() bool) { ownsPaneBus = was }(ownsPaneBus)
	ownsPaneBus = func() bool { return true }
	a := &app{paneID: "pane-1", registered: make(chan struct{})}
	start := time.Now()
	got := a.instructions()
	if waited := time.Since(start); waited > registerWait+time.Second {
		t.Fatalf("initialize waited %s for a registration that never came", waited)
	}
	if !strings.HasPrefix(got, serverInstructions) || strings.Contains(got, buildOrFile) {
		t.Fatal("without a registration the session must still get the fixed rules, and no window parts")
	}
}

// A repo's planner always files and never builds, so it is not told to ask
// "Build it now, or file it as a task?": the person chose by pressing Make
// task. It keeps the to-do rule, and reads the planner's rule for the
// window's repos.
func TestAPlannerIsNotAskedToOfferBuildOrFile(t *testing.T) {
	reader, sessions := `{"agent":false,"planner":true,"self":"app"}`, `[{"name":"app","repoPath":"/r/app","status":"live"},{"name":"backend","repoPath":"/r/backend","status":"cold"}]`
	srv := sessionsServer(t, &reader, &sessions)
	a := &app{daemon: newDaemonClient(srv.URL, "t"), id: "p1", paneID: "pane-1"}
	got := a.instructions()
	if strings.Contains(got, buildOrFile) {
		t.Fatalf("a planner was told to offer build-or-file:\n%s", got)
	}
	for _, want := range []string{"add it with the todo tool, unasked", "you are this repo's planner", "helper of your own"} {
		if !strings.Contains(got, want) {
			t.Errorf("a planner's instructions lost %q", want)
		}
	}
	if strings.Contains(got, "delegate it to that session") {
		t.Fatal("a planner was told to hand work to a repo's own session")
	}
}
