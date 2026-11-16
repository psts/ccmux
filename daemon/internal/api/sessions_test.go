package api

import (
	"context"
	"errors"
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
	mediaDir string          // where MediaDir points every session
	prompts  []runner.Prompt // what SendPrompt was given
	created  []runner.NewSession
	sent     []string
	sentAs   []string // "session|prompt id|sender|text"
	replies  []runner.CardReply
	wanted   []string
	models   []string
	restarts int
	// livenessErr, when set, is what asking where a session is fails with.
	livenessErr error
}

func (f *fakeRunners) Create(n runner.NewSession) (store.RunnerSession, error) {
	f.created = append(f.created, n)
	return store.RunnerSession{ID: "s1", Cwd: n.Cwd, Wanted: store.SessionRunning, WorkspaceID: n.WorkspaceID, PaneID: "pane-of-s1"}, nil
}

func (f *fakeRunners) MediaDir(string) string { return f.mediaDir }

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

func (f *fakeRunners) Restart(id string) error {
	if id == "ended" {
		return runner.ErrSessionDone
	}
	f.restarts++
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

func (fakeRunnerRows) RunnerSessions() ([]store.RunnerSession, error) { return nil, nil }
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
