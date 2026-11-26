package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"ccmux.dev/ccmuxd/internal/manager"
	"ccmux.dev/ccmuxd/internal/model"
	"ccmux.dev/ccmuxd/internal/runner"
	"ccmux.dev/ccmuxd/internal/store"
	"ccmux.dev/ccmuxd/internal/tmux"
)

type fakeRunners struct {
	mediaDir    string          // where MediaDir points every session
	prompts     []runner.Prompt // what SendPrompt was given
	created     []runner.NewSession
	sent        []string
	sentAs      []string // "session|prompt id|sender|text"
	replies     []runner.CardReply
	wanted      []string
	models      []string
	advisors    []string
	efforts     []string
	restarts    int
	startedOver []string // session ids StartOver was asked for
	rewinds     []string // "session to dry" of each going back
	// livenessErr, when set, is what asking where a session is fails with.
	livenessErr error
}

func (f *fakeRunners) Create(n runner.NewSession) (store.RunnerSession, error) {
	f.created = append(f.created, n)
	return store.RunnerSession{ID: "s1", Cwd: n.Cwd, Wanted: store.SessionRunning, WorkspaceID: n.WorkspaceID, PaneID: "pane-of-s1"}, nil
}

func (f *fakeRunners) MediaDir(string) string { return f.mediaDir }

func (f *fakeRunners) Sleep(id string, fresh bool) error {
	f.wanted = append(f.wanted, fmt.Sprintf("%s=sleep fresh=%v", id, fresh))
	return nil
}

func (f *fakeRunners) SetFresh(id string, fresh bool) error {
	f.wanted = append(f.wanted, fmt.Sprintf("%s=fresh %v", id, fresh))
	return nil
}

func (f *fakeRunners) SendPrompt(id string, p runner.Prompt) (string, error) {
	if id != "s1" {
		return "", store.ErrNoSession
	}
	f.prompts = append(f.prompts, p)
	return "p-img", nil
}

func (f *fakeRunners) SendAs(id, promptID, text, sender string) (string, error) {
	if id == "ended" {
		return "", runner.ErrSessionDone
	}
	f.sentAs = append(f.sentAs, id+"|"+promptID+"|"+sender+"|"+text)
	return promptID, nil
}

func (f *fakeRunners) Liveness(id string) (string, error) {
	if f.livenessErr != nil {
		return "", f.livenessErr
	}
	if id == "s1" {
		return store.SessionRunning, nil
	}
	return store.SessionDone, nil
}

func (f *fakeRunners) Rewind(_ context.Context, id, to string, dry bool) (runner.Rewind, error) {
	f.rewinds = append(f.rewinds, fmt.Sprintf("%s %s %v", id, to, dry))
	switch to {
	case "busy":
		return runner.Rewind{}, fmt.Errorf("%w: Claude is working; stop the turn first", runner.ErrRewindRefused)
	case "down":
		return runner.Rewind{}, errors.New("dial unix: no such file")
	case "disk":
		return runner.Rewind{}, fmt.Errorf("%w: the conversation could not be cut: disk full", runner.ErrRewindFailed)
	}
	if id != "s1" {
		return runner.Rewind{}, store.ErrNoSession
	}
	return runner.Rewind{Dropped: 1, Conversation: map[bool]string{false: "fork-1"}[dry]}, nil
}

func (f *fakeRunners) ReplyCard(_ context.Context, id string, r runner.CardReply) (bool, error) {
	f.replies = append(f.replies, r)
	return r.ID != "gone1", nil
}

// sessionsServer is a server over a real registry and a manager holding one
// closed repo session for repo (no tmux session exists for it, so the
// manager adopts it closed), with a fake supervisor behind the routes.
func sessionsServer(t *testing.T, repo string) (*Server, *fakeRunners, *store.SQLite) {
	t.Helper()
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux not installed")
	}
	tsrv := &tmux.Server{Socket: "ccmux-sessions-itest", ConfigPath: "../../config/tmux.conf"}
	_ = tsrv.KillServer()
	t.Cleanup(func() { _ = tsrv.KillServer() })
	st, err := store.Open(filepath.Join(t.TempDir(), "reg.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	ws := &model.Workspace{ID: "ws-app", Name: "app", RepoPath: repo, TmuxSession: "ccmux-app-itest"}
	if err := st.SaveWorkspace(ws); err != nil {
		t.Fatal(err)
	}
	if err := st.SavePane(&model.Pane{ID: "tmux1", WorkspaceID: "ws-app", CWD: repo, Harness: "claude"}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	mgr := manager.New(ctx, tsrv, st)
	if err := mgr.Start(); err != nil {
		t.Fatal(err)
	}
	s := NewServer(mgr)
	f := &fakeRunners{}
	s.SetRunners(f, fakeRunnerRows{})
	return s, f, st
}

func (f *fakeRunners) Send(id, text, sender string) (string, error) {
	if id == "ended" {
		return "", runner.ErrSessionDone
	}
	if id != "s1" {
		return "", store.ErrNoSession
	}
	f.sent = append(f.sent, text)
	return "p1", nil
}

func (f *fakeRunners) SetWanted(id, wanted string) error {
	if id == "ended" {
		return runner.ErrSessionDone
	}
	f.wanted = append(f.wanted, wanted)
	return nil
}

func (f *fakeRunners) RestartBetweenTurns(id string) error { return f.Restart(id) }

func (f *fakeRunners) StartOver(id string) error {
	switch id {
	case "ended":
		return runner.ErrSessionDone
	case "busy":
		return runner.ErrCleanRefused
	}
	f.startedOver = append(f.startedOver, id)
	return nil
}

func (f *fakeRunners) Restart(id string) error {
	if id == "ended" {
		return runner.ErrSessionDone
	}
	f.restarts++
	return nil
}

func (f *fakeRunners) SetAdvisor(id, advisor string) error {
	if id == "ended" {
		return runner.ErrSessionDone
	}
	f.advisors = append(f.advisors, advisor)
	return nil
}

func (f *fakeRunners) SetEffort(id, effort string) error {
	if id == "ended" {
		return runner.ErrSessionDone
	}
	f.efforts = append(f.efforts, effort)
	return nil
}

func (f *fakeRunners) SetModel(id, model string) error {
	if id == "ended" {
		return runner.ErrSessionDone
	}
	f.models = append(f.models, model)
	return nil
}

type fakeRunnerRows struct{}

func (fakeRunnerRows) RunnerSessions() ([]store.RunnerSession, error)    { return nil, nil }
func (fakeRunnerRows) AgentSession(string) (*store.RunnerSession, error) { return nil, nil }
func (fakeRunnerRows) RunnerSession(id string) (*store.RunnerSession, error) {
	if id != "s1" {
		return nil, nil
	}
	return &store.RunnerSession{ID: "s1", PaneID: "run1", WorkspaceID: "ws-app"}, nil
}
func (fakeRunnerRows) UndeliveredPrompts(string) ([]store.InboxPrompt, error) {
	return []store.InboxPrompt{{ID: "p1"}}, nil
}
func (fakeRunnerRows) OpenSessionCards(string) ([]store.SessionCard, error)    { return nil, nil }
func (fakeRunnerRows) SetSessionCardState(string, string, string, int64) error { return nil }
func (fakeRunnerRows) RunnerLead(string) (*store.RunnerSession, error)         { return nil, nil }
func (fakeRunnerRows) SetRunnerSessionTitle(string, string, int64) error       { return nil }
func (fakeRunnerRows) SetRunnerSessionGoal(string, string, int64) error        { return nil }
func (fakeRunnerRows) RunnerPlanners() ([]store.RunnerSession, error)          { return nil, nil }
func (fakeRunnerRows) LastPlannerModel(string) (string, error)                 { return "", nil }

func callSessions(t *testing.T, s *Server, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, httptest.NewRequest(method, path, strings.NewReader(body)))
	return w
}

func TestSessionRoutesAreOffWithoutRunners(t *testing.T) {
	s := &Server{}
	if w := callSessions(t, s, "GET", "/v1/sessions", ""); w.Code != http.StatusServiceUnavailable {
		t.Fatalf("off without a supervisor: %d", w.Code)
	}
}

func TestSessionRoutes(t *testing.T) {
	dir := t.TempDir()
	s, f, _ := sessionsServer(t, dir)
	w := callSessions(t, s, "POST", "/v1/sessions", `{"cwd":"`+dir+`"}`)
	if w.Code != http.StatusCreated || !strings.Contains(w.Body.String(), `"state":"running"`) ||
		!strings.Contains(w.Body.String(), `"workspaceId":"ws-app"`) || !strings.Contains(w.Body.String(), `"paneId":"pane-of-s1"`) {
		t.Fatalf("create answers the session with its pane, its workspace and where it is: %d %s", w.Code, w.Body)
	}
	for _, bad := range []string{`{"cwd":"rel/dir"}`, `{"cwd":"/no/such/folder/here"}`} {
		if w := callSessions(t, s, "POST", "/v1/sessions", bad); w.Code != http.StatusBadRequest {
			t.Errorf("%s: want 400, got %d", bad, w.Code)
		}
	}
	if w := callSessions(t, s, "GET", "/v1/sessions", ""); w.Code != 200 || !strings.Contains(w.Body.String(), `"sessions":[]`) {
		t.Fatalf("list: %d %s", w.Code, w.Body)
	}
	if w := callSessions(t, s, "GET", "/v1/sessions/s1", ""); w.Code != 200 || !strings.Contains(w.Body.String(), `"waiting":1`) || !strings.Contains(w.Body.String(), `"state":"running"`) {
		t.Fatalf("one: %d %s", w.Code, w.Body)
	}
	if w := callSessions(t, s, "GET", "/v1/sessions/nope", ""); w.Code != http.StatusNotFound {
		t.Fatalf("unknown: %d", w.Code)
	}
	if w := callSessions(t, s, "POST", "/v1/sessions/s1/prompt", `{"text":"hi"}`); w.Code != http.StatusAccepted {
		t.Fatalf("prompt: %d %s", w.Code, w.Body)
	}
	if w := callSessions(t, s, "POST", "/v1/sessions/nope/prompt", `{"text":"hi"}`); w.Code != http.StatusNotFound {
		t.Fatalf("prompt to unknown: %d", w.Code)
	}
	if w := callSessions(t, s, "POST", "/v1/sessions/s1/wanted", `{"wanted":"sideways"}`); w.Code != http.StatusBadRequest {
		t.Fatalf("bad wanted: %d", w.Code)
	}
	for _, path := range []string{"/v1/sessions/ended/prompt", "/v1/sessions/ended/wanted"} {
		body := `{"text":"hi","wanted":"running"}`
		if w := callSessions(t, s, "POST", path, body); w.Code != http.StatusConflict {
			t.Errorf("%s on an ended session: want 409, got %d", path, w.Code)
		}
	}
	callSessions(t, s, "POST", "/v1/sessions/s1/wanted", `{"wanted":"asleep"}`)
	w = callSessions(t, s, "POST", "/v1/sessions/s1/model", `{"model":"haiku"}`)
	if w.Code != 200 || !strings.Contains(w.Body.String(), "on the next restart") || !strings.Contains(w.Body.String(), `"restart":"/v1/sessions/s1/restart"`) {
		t.Fatalf("a model says it applies on the next restart and names the restart: %d %s", w.Code, w.Body)
	}
	if w := callSessions(t, s, "POST", "/v1/sessions/s1/restart", ``); w.Code != http.StatusAccepted || f.restarts != 1 {
		t.Fatalf("restart now: %d %d", w.Code, f.restarts)
	}
	if w := callSessions(t, s, "POST", "/v1/sessions/ended/restart", ``); w.Code != http.StatusConflict {
		t.Fatalf("an ended session does not restart: %d", w.Code)
	}
	if w := callSessions(t, s, "POST", "/v1/sessions/ended/model", `{"model":"haiku"}`); w.Code != http.StatusConflict {
		t.Fatalf("an ended session takes no model: %d", w.Code)
	}
	if len(f.created) != 1 || len(f.sent) != 1 || len(f.wanted) != 1 || len(f.models) != 1 {
		t.Fatalf("each call reached the supervisor once: %+v", f)
	}
}

// A session joins a repo workspace: found by its folder, or named. One that
// no workspace holds is refused, since it would be on no bus.
func TestANewSessionJoinsItsReposWorkspace(t *testing.T) {
	repo := t.TempDir()
	elsewhere := t.TempDir()
	s, f, _ := sessionsServer(t, repo)
	for body, want := range map[string]int{
		`{"cwd":"` + repo + `"}`:                                 http.StatusCreated,
		`{"cwd":"` + elsewhere + `","workspaceId":"ws-app"}`:     http.StatusCreated, // a worktree outside the repo names it
		`{"cwd":"` + elsewhere + `"}`:                            http.StatusConflict,
		`{"cwd":"` + elsewhere + `","workspaceId":"no-such-ws"}`: http.StatusBadRequest,
	} {
		if w := callSessions(t, s, "POST", "/v1/sessions", body); w.Code != want {
			t.Errorf("%s: got %d, want %d (%s)", body, w.Code, want, w.Body)
		}
	}
	if len(f.created) != 2 {
		t.Fatalf("only the two with a workspace reached the supervisor: %+v", f.created)
	}
	for _, n := range f.created {
		if n.WorkspaceID != "ws-app" {
			t.Errorf("created in %q, want ws-app: %+v", n.WorkspaceID, n)
		}
	}
	if w := callSessions(t, s, "POST", "/v1/sessions", `{"cwd":"`+elsewhere+`"}`); !strings.Contains(w.Body.String(), "open the repo in ccmux first") {
		t.Fatalf("the refusal says what to do: %s", w.Body)
	}
}

// A session whose state could not be read is served as unknown, with its
// row: never as done or asleep, which a caller would act on.
func TestAStateThatCouldNotBeReadIsServedAsUnknown(t *testing.T) {
	s, f, _ := sessionsServer(t, t.TempDir())
	if w := callSessions(t, s, "GET", "/v1/sessions/s1", ""); !strings.Contains(w.Body.String(), `"state":"running"`) {
		t.Fatalf("read: %s", w.Body)
	}
	f.livenessErr = errors.New("database is locked")
	w := callSessions(t, s, "GET", "/v1/sessions/s1", "")
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"state":"unknown"`) || !strings.Contains(w.Body.String(), `"id":"s1"`) {
		t.Fatalf("not read: %d %s", w.Code, w.Body)
	}
	for _, guess := range []string{`"state":"done"`, `"state":"asleep"`, `"state":"running"`} {
		if strings.Contains(w.Body.String(), guess) {
			t.Fatalf("served a guess %s: %s", guess, w.Body)
		}
	}
}

func TestRewindRoute(t *testing.T) {
	s, f, _ := sessionsServer(t, t.TempDir())
	if w := callSessions(t, s, "POST", "/v1/sessions/s1/rewind", `{"to":"p2","dry":true}`); w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"dropped":1`) || strings.Contains(w.Body.String(), "fork-1") {
		t.Fatalf("a look: %d %s", w.Code, w.Body)
	}
	if w := callSessions(t, s, "POST", "/v1/sessions/s1/rewind", `{"to":"p2"}`); w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"conversation":"fork-1"`) {
		t.Fatalf("going back: %d %s", w.Code, w.Body)
	}
	cases := []struct {
		path, body string
		code       int
		says       string
	}{
		{"/v1/sessions/s1/rewind", `{"to":"busy"}`, http.StatusConflict, `"Claude is working; stop the turn first"`},
		{"/v1/sessions/s1/rewind", `{"to":"down"}`, http.StatusBadGateway, "going back failed"},
		{"/v1/sessions/s1/rewind", `{"to":"disk"}`, http.StatusBadGateway, `"going back failed: the conversation could not be cut: disk full"`},
		{"/v1/sessions/nope/rewind", `{"to":"p1"}`, http.StatusNotFound, ""},
		{"/v1/sessions/s1/rewind", `{}`, http.StatusBadRequest, "required"},
	}
	for _, c := range cases {
		if w := callSessions(t, s, "POST", c.path, c.body); w.Code != c.code || !strings.Contains(w.Body.String(), c.says) {
			t.Errorf("%s %s: want %d %q, got %d %s", c.path, c.body, c.code, c.says, w.Code, w.Body)
		}
	}
	if len(f.rewinds) != 6 || f.rewinds[1] != "s1 p2 false" {
		t.Fatalf("what the supervisor was asked: %v", f.rewinds)
	}
}

// A message that asks for a clean start carries its key to the runner,
// text only or not; one without stays on the plain path.
func TestAPromptCarriesItsCleanStart(t *testing.T) {
	s, f, _ := sessionsServer(t, t.TempDir())
	if w := callSessions(t, s, "POST", "/v1/sessions/s1/prompt", `{"text":"start over","sender":"you","clean":"k1"}`); w.Code != http.StatusAccepted {
		t.Fatalf("clean prompt: %d %s", w.Code, w.Body)
	}
	if len(f.prompts) != 1 || f.prompts[0].Clean != "k1" || f.prompts[0].Text != "start over" {
		t.Fatalf("the runner was given %+v", f.prompts)
	}
	callSessions(t, s, "POST", "/v1/sessions/s1/prompt", `{"text":"go on"}`)
	if len(f.prompts) != 1 {
		t.Fatalf("a plain message took the clean path: %+v", f.prompts)
	}
}

// The bus's StartMain makes a repo's main session the way the thread page
// does for a repo that has none: kind lead, in the repo's folder, on the
// default model. Anything that is no repo session on this host is refused.
func TestTheBusStartsARepoMainSession(t *testing.T) {
	dir := t.TempDir()
	s, f, _ := sessionsServer(t, dir)
	pane, cwd, err := s.startMainForBus("ws-app")
	if err != nil || pane != "pane-of-s1" || cwd != dir {
		t.Fatalf("start = %q %q %v, want the new session's pane in %s", pane, cwd, err, dir)
	}
	if len(f.created) != 1 || f.created[0].Kind != store.SessionLead || f.created[0].WorkspaceID != "ws-app" || f.created[0].Cwd != dir || f.created[0].Model != "" {
		t.Fatalf("made %+v, want one lead in the repo's folder on the default model", f.created)
	}
	if _, _, err := s.startMainForBus("no-such-ws"); err == nil || len(f.created) != 1 {
		t.Fatalf("an unknown workspace: err %v, made %d; want a refusal and nothing made", err, len(f.created))
	}
}
