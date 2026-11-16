package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"

	"ccmux.dev/ccmuxd/internal/runner"
	"ccmux.dev/ccmuxd/internal/store"
)

// threadRows is the session rows in memory, with the one-lead rule the
// registry's index keeps.
type threadRows struct {
	fakeRunnerRows
	mu       sync.Mutex
	sessions map[string]*store.RunnerSession
	// blindOnce makes the next look for a lead find none, as it does when
	// another window's insert lands between that look and ours.
	blindOnce bool
}

func (r *threadRows) RunnerSession(id string) (*store.RunnerSession, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if s := r.sessions[id]; s != nil {
		c := *s
		return &c, nil
	}
	return nil, nil
}

func (r *threadRows) lead(wsID string) *store.RunnerSession {
	for _, s := range r.sessions {
		if s.WorkspaceID == wsID && s.Kind == store.SessionLead {
			c := *s
			return &c
		}
	}
	return nil
}

func (r *threadRows) RunnerLead(wsID string) (*store.RunnerSession, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.blindOnce {
		r.blindOnce = false
		return nil, nil
	}
	return r.lead(wsID), nil
}

func (r *threadRows) set(id string, change func(*store.RunnerSession)) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if s := r.sessions[id]; s != nil {
		change(s)
		return nil
	}
	return store.ErrNoSession
}

func (r *threadRows) SetRunnerSessionTitle(id, title string, _ int64) error {
	return r.set(id, func(s *store.RunnerSession) { s.Title = title })
}

func (r *threadRows) SetRunnerSessionGoal(id, goal string, _ int64) error {
	return r.set(id, func(s *store.RunnerSession) { s.Goal = goal })
}

// threadSup makes sessions into threadRows.
type threadSup struct {
	*fakeRunners
	rows *threadRows
}

func (f *threadSup) Create(n runner.NewSession) (store.RunnerSession, error) {
	f.rows.mu.Lock()
	defer f.rows.mu.Unlock()
	if n.Kind == store.SessionLead && f.rows.lead(n.WorkspaceID) != nil {
		return store.RunnerSession{}, store.ErrLeadExists
	}
	sess := store.RunnerSession{ID: fmt.Sprintf("n%d", len(f.rows.sessions)+1), Cwd: n.Cwd, Wanted: store.SessionRunning, WorkspaceID: n.WorkspaceID, PaneID: fmt.Sprintf("pane%d", len(f.rows.sessions)+1), Kind: n.Kind, Title: n.Title}
	f.rows.sessions[sess.ID] = &sess
	return sess, nil
}

func (f *threadSup) Send(id, text, _ string) (string, error) {
	if s, _ := f.rows.RunnerSession(id); s == nil {
		return "", store.ErrNoSession
	}
	return "p1", nil
}

func (f *threadSup) Liveness(string) (string, error) { return store.SessionRunning, nil }

func threadServer(t *testing.T) (*Server, *threadRows, string) {
	t.Helper()
	repo := t.TempDir()
	s, f, _ := sessionsServer(t, repo)
	rows := &threadRows{sessions: map[string]*store.RunnerSession{}}
	s.SetRunners(&threadSup{fakeRunners: f, rows: rows}, rows)
	return s, rows, repo
}

func made(t *testing.T, s *Server, body string) (int, store.RunnerSession) {
	t.Helper()
	w := callSessions(t, s, "POST", "/v1/sessions", body)
	var sess store.RunnerSession
	json.Unmarshal(w.Body.Bytes(), &sess)
	return w.Code, sess
}

func TestARepoHasOneLeadHoweverOftenItIsAskedFor(t *testing.T) {
	s, rows, repo := threadServer(t)
	body := `{"cwd":"` + repo + `","kind":"lead"}`
	code, first := made(t, s, body)
	if code != http.StatusCreated || first.Kind != store.SessionLead || first.WorkspaceID != "ws-app" {
		t.Fatalf("the first ask makes it: %d %+v", code, first)
	}
	if code, again := made(t, s, body); code != http.StatusOK || again.ID != first.ID {
		t.Fatalf("the second ask is answered with it: %d %+v", code, again)
	}
	// Two windows at once: this one looked before the other's insert landed.
	rows.blindOnce = true
	if code, raced := made(t, s, body); code != http.StatusOK || raced.ID != first.ID {
		t.Fatalf("losing the race is answered with the winner's lead: %d %+v", code, raced)
	}
	// Threads are made new each time, beside the lead.
	_, t1 := made(t, s, `{"cwd":"`+repo+`","title":"  Trade CSV export\nsecond line  "}`)
	code, t2 := made(t, s, `{"cwd":"`+repo+`"}`)
	if code != http.StatusCreated || t1.ID == t2.ID || t1.Kind != "" || t1.Title != "Trade CSV export" {
		t.Fatalf("threads: %d %+v %+v", code, t1, t2)
	}
	if len(rows.sessions) != 3 {
		t.Fatalf("one lead and two threads: %d sessions", len(rows.sessions))
	}
	if code, _ := made(t, s, `{"cwd":"`+repo+`","kind":"helper"}`); code != http.StatusBadRequest {
		t.Fatalf("a kind this stage does not have: %d", code)
	}
}

func TestAThreadIsNamedAfterItsFirstPromptAndKeepsAGivenName(t *testing.T) {
	s, rows, repo := threadServer(t)
	_, lead := made(t, s, `{"cwd":"`+repo+`","kind":"lead"}`)
	_, thread := made(t, s, `{"cwd":"`+repo+`"}`)
	_, named := made(t, s, `{"cwd":"`+repo+`","title":"Drawings lost on exit"}`)
	sub, events := s.mgr.SubscribeEvents()
	defer s.mgr.UnsubscribeEvents(sub)

	long := "Users want to export closed trades like this screen shows. " + strings.Repeat("And more. ", 20)
	for _, id := range []string{lead.ID, thread.ID, named.ID} {
		if w := callSessions(t, s, "POST", "/v1/sessions/"+id+"/prompt", `{"text":`+fmt.Sprintf("%q", long+"\nline two")+`}`); w.Code != http.StatusAccepted {
			t.Fatalf("%s: %d %s", id, w.Code, w.Body)
		}
	}
	got := rows.sessions[thread.ID].Title
	if !strings.HasPrefix(got, "Users want to export closed trades") || len([]rune(got)) > maxSessionTitle || !strings.HasSuffix(got, "…") || strings.Contains(got, "line two") {
		t.Fatalf("the thread's title is its first prompt's first line, cut: %q", got)
	}
	if ev := <-events; ev.Kind != "sessions-changed" || ev.WorkspaceID != "ws-app" {
		t.Fatalf("the lenses are told: %+v", ev)
	}
	if rows.sessions[lead.ID].Title != "" || rows.sessions[named.ID].Title != "Drawings lost on exit" {
		t.Fatalf("a lead is not named after a prompt, and a given name stays: %q %q", rows.sessions[lead.ID].Title, rows.sessions[named.ID].Title)
	}
	callSessions(t, s, "POST", "/v1/sessions/"+thread.ID+"/prompt", `{"text":"something else entirely"}`)
	if rows.sessions[thread.ID].Title != got {
		t.Fatalf("a later prompt does not rename it: %q", rows.sessions[thread.ID].Title)
	}
	// And a human can rename it.
	w := callSessions(t, s, "POST", "/v1/sessions/"+thread.ID+"/title", `{"title":"  CSV export  "}`)
	if w.Code != http.StatusOK || rows.sessions[thread.ID].Title != "CSV export" {
		t.Fatalf("rename: %d %s %q", w.Code, w.Body, rows.sessions[thread.ID].Title)
	}
	if w := callSessions(t, s, "POST", "/v1/sessions/nope/title", `{"title":"x"}`); w.Code != http.StatusNotFound {
		t.Fatalf("no such session: %d", w.Code)
	}
}

func TestWhatASessionTellsTheBusItIsDoingIsKeptAsItsGoal(t *testing.T) {
	s, rows, repo := threadServer(t)
	_, lead := made(t, s, `{"cwd":"`+repo+`","kind":"lead"}`)
	s.mgr.SyncRunnerPane(*rows.sessions[lead.ID]) // its pane, as the supervisor's Changed makes it
	sub, events := s.mgr.SubscribeEvents()
	defer s.mgr.UnsubscribeEvents(sub)

	s.keepRunnerGoal(lead.PaneID, "CSV export: backend done, admin table next.")
	if rows.sessions[lead.ID].Goal != "CSV export: backend done, admin table next." {
		t.Fatalf("goal: %q", rows.sessions[lead.ID].Goal)
	}
	if ev := <-events; ev.Kind != "sessions-changed" {
		t.Fatalf("the lenses are told: %+v", ev)
	}
	s.keepRunnerGoal("tmux1", "a terminal pane's summary") // not a runner's pane: nothing to keep it on
	s.keepRunnerGoal("no-such-pane", "x")
	if rows.sessions[lead.ID].Goal != "CSV export: backend done, admin table next." {
		t.Fatalf("another pane's summary changed it: %q", rows.sessions[lead.ID].Goal)
	}
	w := callSessions(t, s, "GET", "/v1/sessions/"+lead.ID, "")
	if !strings.Contains(w.Body.String(), `"goal":"CSV export: backend done, admin table next."`) || !strings.Contains(w.Body.String(), `"kind":"lead"`) {
		t.Fatalf("served with the session: %s", w.Body)
	}
}
