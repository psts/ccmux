package api

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"ccmux.dev/ccmuxd/internal/store"
	"ccmux.dev/ccmuxd/internal/threads"
)

// fakeThreads is internal/threads as the routes see it.
type fakeThreads struct {
	made      []threads.New
	createErr error
	edits     []string // session ids asked to move
	editErr   error
}

func (f *fakeThreads) Create(n threads.New) (store.Thread, store.RunnerSession, error) {
	if f.createErr != nil {
		return store.Thread{}, store.RunnerSession{}, f.createErr
	}
	f.made = append(f.made, n)
	return store.Thread{ID: 7, Title: n.Title, State: store.ThreadOpen, LeadSession: "lead1"},
		store.RunnerSession{ID: "lead1", Name: "app-thread-7", ThreadID: 7, Role: store.RoleLead, ReadOnly: true, Wanted: store.SessionRunning}, nil
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
