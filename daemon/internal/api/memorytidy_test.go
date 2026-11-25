package api

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"ccmux.dev/ccmuxd/internal/memtidy"
)

type memoryTidyAnswer struct {
	On      *bool          `json:"on"`
	Running bool           `json:"running"`
	Last    *memtidy.Night `json:"last"`
}

func memoryTidyCall(t *testing.T, s *Server, method, body string, want int) memoryTidyAnswer {
	t.Helper()
	w := callSessions(t, s, method, "/v1/workspaces/ws-app/memory-tidy", body)
	if w.Code != want {
		t.Fatalf("%s %s: %d %s, want %d", method, body, w.Code, w.Body, want)
	}
	var out memoryTidyAnswer
	if want == http.StatusOK {
		if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
			t.Fatal(err)
		}
	}
	return out
}

func TestMemoryTidyRoutesAreOffWithoutSwitches(t *testing.T) {
	s := &Server{}
	if w := callSessions(t, s, "GET", "/v1/workspaces/ws-app/memory-tidy", ""); w.Code != http.StatusServiceUnavailable {
		t.Fatalf("got %d", w.Code)
	}
}

// A repo is on until switched off, the switch is kept by the repo's
// folder, and last night comes from the tidy-up's log.
func TestMemoryTidySwitchRoundTrip(t *testing.T) {
	repo := t.TempDir()
	s, _, st := sessionsServer(t, repo)
	state := t.TempDir()
	s.SetMemoryTidy(&memtidy.Switches{Settings: st, StateDir: state}, true)

	got := memoryTidyCall(t, s, "GET", "", http.StatusOK)
	if got.On == nil || !*got.On || !got.Running || got.Last != nil {
		t.Fatalf("a repo starts on with no night: %+v", got)
	}
	memoryTidyCall(t, s, "PUT", `{}`, http.StatusBadRequest)
	if got = memoryTidyCall(t, s, "PUT", `{"on":false}`, http.StatusOK); *got.On {
		t.Fatalf("switched off: %+v", got)
	}
	if v, _ := st.GetSetting("memory-tidy:" + repo); v != "off" {
		t.Fatalf("the setting is the repo's: %q", v)
	}
	line := `{"at":"2026-11-25T03:00:00Z","repo":"` + repo + `","conversations":4,"result":"applied","costUSD":0.42,"todos":1}` + "\n"
	os.WriteFile(filepath.Join(state, "log.jsonl"), []byte(line), 0o600)
	got = memoryTidyCall(t, s, "PUT", `{"on":true}`, http.StatusOK)
	if !*got.On || got.Last == nil || got.Last.CostUSD != 0.42 || got.Last.Conversations != 4 || got.Last.Todos != 1 {
		t.Fatalf("on again, with last night: %+v %+v", got, got.Last)
	}
	if w := callSessions(t, s, "GET", "/v1/workspaces/nope/memory-tidy", ""); w.Code != http.StatusNotFound {
		t.Fatalf("an unknown workspace: %d", w.Code)
	}
}

func TestAddTodoFilesOnTheRepoList(t *testing.T) {
	s, _ := todoServer(t)
	repo := t.TempDir()
	if err := s.AddTodo(repo, "tests flake: add a retry note to CLAUDE.md", "low", "memory tidy-up"); err != nil {
		t.Fatal(err)
	}
	list, err := s.todos.List(repo)
	if err != nil || len(list) != 1 || list[0].AddedBy != "memory tidy-up" || list[0].Urgency != "low" {
		t.Fatalf("list: %+v %v", list, err)
	}
	if err := (&Server{}).AddTodo(repo, "x", "low", "y"); err == nil {
		t.Fatal("no to-do lists is an error")
	}
}
