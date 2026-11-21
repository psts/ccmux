package api

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"ccmux.dev/ccmuxd/internal/store"
	"ccmux.dev/ccmuxd/internal/taskq"
	"ccmux.dev/ccmuxd/internal/threads"
)

// fakeThreads is internal/threads as the routes see it.
type fakeThreads struct {
	made      []threads.New
	createErr error
	edits     []string // session ids asked to move
	editErr   error
	acts      []string // merge, end, stand-down and tool calls, in order
	refuse    error    // what the next action is refused with
	changed   []string // session ids LeadChanged heard of
	mu        sync.Mutex
	movedOn   []string // "session why" per MainMovedOn
	built     []taskq.Builder
}

func (f *fakeThreads) StartBuilder(b taskq.Builder) (string, error) {
	f.built = append(f.built, b)
	return "pane-" + b.Name, nil
}

func (f *fakeThreads) Create(n threads.New) (store.Thread, store.RunnerSession, error) {
	if f.createErr != nil {
		return store.Thread{}, store.RunnerSession{}, f.createErr
	}
	f.made = append(f.made, n)
	return store.Thread{ID: 7, Title: n.Title, State: store.ThreadOpen, LeadSession: "lead1"},
		store.RunnerSession{ID: "lead1", Name: "app-thread-7", ThreadID: 7, Role: store.RoleLead, ReadOnly: true, Wanted: store.SessionRunning}, nil
}

func (f *fakeThreads) List() ([]store.Thread, error) {
	return []store.Thread{{ID: 7, Title: "Trade CSV export", State: store.ThreadOpen, LeadSession: "lead1"}}, nil
}

func (f *fakeThreads) Detail(id int64) (threads.Detail, error) {
	if id != 7 {
		return threads.Detail{}, store.ErrNoThread
	}
	return threads.Detail{Thread: store.Thread{ID: 7, State: store.ThreadMerging}, Members: []threads.Member{{Repo: "app", Status: "Reading only"}},
		Merge: &threads.MergeView{Job: "merge-thread-7-ab12cd", Unit: "ccmux-job@merge-thread-7-ab12cd.service", State: "running", Doing: "app: checks running", StartedAt: 5, Attempts: 1}}, nil
}

// act notes an action on a thread and answers as the service would.
func (f *fakeThreads) act(what string, id int64) (string, error) {
	if id != 7 {
		return "", store.ErrNoThread
	}
	if f.refuse != nil {
		return "", f.refuse
	}
	f.acts = append(f.acts, what)
	return "did " + what, nil
}

func (f *fakeThreads) Merge(id int64, by string) (string, error) { return f.act("merge by "+by, id) }
func (f *fakeThreads) End(id int64, by string) (string, error)   { return f.act("end by "+by, id) }
func (f *fakeThreads) StopMerge(id int64, by string) (string, error) {
	return f.act("stop-merge by "+by, id)
}
func (f *fakeThreads) StandDown(id int64, repo, by string) (string, error) {
	return f.act("stand-down "+repo+" by "+by, id)
}

func (f *fakeThreads) Tool(sessionID, action, repo string) (string, error) {
	if sessionID != "lead1" {
		return "", threads.ErrNotMember
	}
	return f.act(strings.TrimSpace("tool "+action+" "+repo), 7)
}

func (f *fakeThreads) LeadChanged(sessionID string) { f.changed = append(f.changed, sessionID) }
func (f *fakeThreads) MainMovedOn(sessionID, why string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.movedOn = append(f.movedOn, sessionID+" "+why)
}

// movedOnNow is what MainMovedOn heard so far (it may be called off the
// request's goroutine).
func (f *fakeThreads) movedOnNow() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.movedOn...)
}

func (f *fakeThreads) FirstEdit(sessionID string) (threads.Moved, error) {
	if f.editErr != nil {
		return threads.Moved{}, f.editErr
	}
	f.edits = append(f.edits, sessionID)
	return threads.Moved{Message: "moving", Notice: "moved"}, nil
}

func threadsAPI(t *testing.T) (*Server, *fakeThreads) {
	t.Helper()
	s, _, _ := sessionsServer(t, t.TempDir())
	f := &fakeThreads{}
	s.SetThreads(f)
	return s, f
}

// fromPane is a call a runner makes on its own pane, from this host or not.
func fromPane(s *Server, path, remote string) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	req := httptest.NewRequest("POST", path, strings.NewReader(`{"tool":"Edit"}`))
	req.RemoteAddr = remote
	s.Handler().ServeHTTP(w, req)
	return w
}

func TestAThreadIsStartedFromARepoSession(t *testing.T) {
	s, f := threadsAPI(t)
	w := callSessions(t, s, "POST", "/v1/threads", `{"workspaceId":"ws-app","title":"  Trade CSV export\nmore ","model":"haiku"}`)
	if w.Code != http.StatusCreated {
		t.Fatalf("a thread is made: %d %s", w.Code, w.Body)
	}
	if len(f.made) != 1 || f.made[0] != (threads.New{WorkspaceID: "ws-app", Title: "Trade CSV export", Model: "haiku"}) {
		t.Fatalf("with its repo, its one-line title and its model: %+v", f.made)
	}
	for _, want := range []string{`"thread":{"id":7`, `"session":{"id":"lead1"`, `"readOnly":true`, `"state":`} {
		if !strings.Contains(w.Body.String(), want) {
			t.Fatalf("the answer has the thread and its lead as the page reads a session, lacks %s: %s", want, w.Body)
		}
	}
	if w := callSessions(t, s, "POST", "/v1/threads", `{}`); w.Code != http.StatusBadRequest {
		t.Fatalf("a thread needs a repo to lead it: %d", w.Code)
	}
	f.createErr = errors.New("not a git repo")
	if w := callSessions(t, s, "POST", "/v1/threads", `{"workspaceId":"ws-app"}`); w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "not a git repo") {
		t.Fatalf("a refusal says why: %d %s", w.Code, w.Body)
	}
}

func TestAThreadIsStartedWithTheAdvisorPicked(t *testing.T) {
	s, f := threadsAPI(t)
	if w := callSessions(t, s, "POST", "/v1/threads", `{"workspaceId":"ws-app","advisor":"--x"}`); w.Code != http.StatusBadRequest || len(f.made) != 0 {
		t.Fatalf("only a listed advisor is taken, before anything is made: %d %s %+v", w.Code, w.Body, f.made)
	}
	if w := callSessions(t, s, "POST", "/v1/threads", `{"workspaceId":"ws-app","advisor":"fable"}`); w.Code != http.StatusCreated || len(f.made) != 1 || f.made[0].Advisor != "fable" {
		t.Fatalf("the thread is asked for with it: %d %s %+v", w.Code, w.Body, f.made)
	}
}

func TestThreadRoutesAreOffWithoutThreads(t *testing.T) {
	s, _, _ := sessionsServer(t, t.TempDir())
	if w := callSessions(t, s, "POST", "/v1/threads", `{"workspaceId":"ws-app"}`); w.Code != http.StatusServiceUnavailable {
		t.Fatalf("off without the service: %d", w.Code)
	}
	if w := callSessions(t, &Server{}, "POST", "/v1/threads", `{}`); w.Code != http.StatusServiceUnavailable {
		t.Fatalf("off without runners: %d", w.Code)
	}
}

func TestAMembersRunnerAsksForItsWorktreeOnItsOwnPane(t *testing.T) {
	s, f := threadsAPI(t)
	s.mgr.SyncRunnerPane(store.RunnerSession{ID: "lead1", PaneID: "pane-lead", WorkspaceID: "ws-app", Wanted: store.SessionRunning, Cwd: "/r/app"})
	w := fromPane(s, "/v1/panes/pane-lead/first-edit", "127.0.0.1:4000")
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"message":"moving"`) || !strings.Contains(w.Body.String(), `"notice":"moved"`) {
		t.Fatalf("the runner gets what to tell Claude now and after the restart: %d %s", w.Code, w.Body)
	}
	if len(f.edits) != 1 || f.edits[0] != "lead1" {
		t.Fatalf("the pane names the session: %v", f.edits)
	}
	if w := fromPane(s, "/v1/panes/pane-lead/first-edit", "100.64.0.9:4000"); w.Code != http.StatusForbidden {
		t.Fatalf("only this host's runners ask: %d", w.Code)
	}
	if w := fromPane(s, "/v1/panes/tmux1/first-edit", "127.0.0.1:4000"); w.Code != http.StatusNotFound {
		t.Fatalf("a pane that is no runner session's is unknown: %d", w.Code)
	}
	f.editErr = threads.ErrNotMember
	if w := fromPane(s, "/v1/panes/pane-lead/first-edit", "127.0.0.1:4000"); w.Code != http.StatusConflict {
		t.Fatalf("a session in no thread is refused, so its runner keeps the call refused: %d", w.Code)
	}
	f.editErr = errors.New("disk full")
	if w := fromPane(s, "/v1/panes/pane-lead/first-edit", "127.0.0.1:4000"); w.Code != http.StatusInternalServerError || !strings.Contains(w.Body.String(), "disk full") {
		t.Fatalf("a worktree that could not be made says why: %d %s", w.Code, w.Body)
	}
}

func TestAThreadIsReadAndActedOnFromItsPage(t *testing.T) {
	s, f := threadsAPI(t)
	if w := callSessions(t, s, "GET", "/v1/threads", ""); w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"threads":[{"id":7`) {
		t.Fatalf("the list is the thread rows: %d %s", w.Code, w.Body)
	}
	if w := callSessions(t, s, "GET", "/v1/threads/7", ""); w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"status":"Reading only"`) {
		t.Fatalf("one thread comes with its members: %d %s", w.Code, w.Body)
	}
	if w := callSessions(t, s, "GET", "/v1/threads/7", ""); !strings.Contains(w.Body.String(), `"merge":{"job":"merge-thread-7-ab12cd","unit":"ccmux-job@merge-thread-7-ab12cd.service","state":"running","doing":"app: checks running","startedAt":5,"attempts":1}`) {
		t.Fatalf("and with its merge job, while one runs: %s", w.Body)
	}
	if w := callSessions(t, s, "GET", "/v1/threads/8", ""); w.Code != http.StatusNotFound {
		t.Fatalf("an unknown thread is not found: %d", w.Code)
	}
	if w := callSessions(t, s, "GET", "/v1/threads/seven", ""); w.Code != http.StatusBadRequest {
		t.Fatalf("a thread is named by its number: %d", w.Code)
	}
	for _, c := range []struct{ path, body, did string }{
		{"/v1/threads/7/merge", "", "merge by person"},
		{"/v1/threads/7/stop-merge", "", "stop-merge by person"},
		{"/v1/threads/7/stand-down", `{"repo":"backend"}`, "stand-down backend by person"},
		{"/v1/threads/7/end", "", "end by person"},
	} {
		if w := callSessions(t, s, "POST", c.path, c.body); w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"message":"did `+c.did+`"`) {
			t.Fatalf("%s answers with what was done: %d %s", c.path, w.Code, w.Body)
		}
	}
	if strings.Join(f.acts, "|") != "merge by person|stop-merge by person|stand-down backend by person|end by person" {
		t.Fatalf("each route is its own action, asked for by a person: %v", f.acts)
	}
	f.refuse = errors.New("backend-thread-7 is still working; wait for it to report, then merge")
	if w := callSessions(t, s, "POST", "/v1/threads/7/merge", ""); w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), "still working") {
		t.Fatalf("a refused action says why: %d %s", w.Code, w.Body)
	}
	if w := callSessions(t, s, "POST", "/v1/threads/8/merge", ""); w.Code != http.StatusNotFound {
		t.Fatalf("an action on an unknown thread is not found: %d", w.Code)
	}
	if len(f.acts) != 4 {
		t.Fatalf("a refused action did nothing: %v", f.acts)
	}
}

func TestTheThreadToolActsForThePanesOwnSession(t *testing.T) {
	s, f := threadsAPI(t)
	s.mgr.SyncRunnerPane(store.RunnerSession{ID: "lead1", PaneID: "pane-lead", WorkspaceID: "ws-app", Wanted: store.SessionRunning, Cwd: "/r/app"})
	s.mgr.SyncRunnerPane(store.RunnerSession{ID: "plain1", PaneID: "pane-plain", WorkspaceID: "ws-app", Wanted: store.SessionRunning, Cwd: "/r/app"})
	w := callSessions(t, s, "POST", "/v1/panes/pane-lead/thread", `{"action":"stand_down","repo":"backend"}`)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"message":"did tool stand_down backend"`) || f.acts[0] != "tool stand_down backend" {
		t.Fatalf("the pane names the session, the body the action: %d %s %v", w.Code, w.Body, f.acts)
	}
	for _, pane := range []string{"pane-plain", "tmux1", "nope"} {
		if w := callSessions(t, s, "POST", "/v1/panes/"+pane+"/thread", `{"action":"merge"}`); w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), "not in a thread") {
			t.Fatalf("%s is no thread member's pane: %d %s", pane, w.Code, w.Body)
		}
	}
	if len(f.acts) != 1 {
		t.Fatalf("nothing was done for them: %v", f.acts)
	}
}

func TestAThreadHearsWhenItsLeadsTitleOrModelChanges(t *testing.T) {
	s, f := threadsAPI(t)
	if w := callSessions(t, s, "POST", "/v1/sessions/s1/model", `{"model":"opus"}`); w.Code != http.StatusOK {
		t.Fatalf("the model is saved: %d %s", w.Code, w.Body)
	}
	if len(f.changed) != 1 || f.changed[0] != "s1" {
		t.Fatalf("the threads service hears whose model changed: %v", f.changed)
	}
	if w := callSessions(t, s, "POST", "/v1/sessions/s1/model", `{}`); w.Code != http.StatusBadRequest || len(f.changed) != 1 {
		t.Fatalf("a model that was not saved is not passed on: %d %v", w.Code, f.changed)
	}
}
