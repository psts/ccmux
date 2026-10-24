package api

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"ccmux.dev/ccmuxd/internal/peers"
	"ccmux.dev/ccmuxd/internal/repoowns"
)

// noticeObserver is a session in the window that the bus counts as present:
// registered on a running agent's pane (a pane at a bare shell has nobody to
// read, so the bus rightly skips it). It is called "tmp" (its git root), so
// it also hears the agent's own join, which inbox below simply passes over.
func noticeObserver(t *testing.T, f *windowAgentFixture) string {
	t.Helper()
	f.srv.EnablePeers(peers.NewService(f.st, f.srv.mgr, testSecret))
	f.srv.mgr.OnMembershipChange = f.srv.WindowNotice
	_, ap := f.start(t, "x-poster", "")
	waitForegroundNotShell(t, f, ap.ID)
	nextFakePID++
	resp := postJSON(t, f.base+"/v1/peers/register", peers.TokenForPane(testSecret, ap.ID),
		map[string]any{"pane_id": ap.ID, "pid": nextFakePID, "cwd": "/tmp", "git_root": "/tmp"})
	var reg struct {
		PeerID string `json:"peer_id"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&reg); err != nil || reg.PeerID == "" {
		t.Fatalf("register: %v", err)
	}
	return reg.PeerID
}

// noticesFor collects what peerID hears for d.
func noticesFor(f *windowAgentFixture, peerID string, d time.Duration) []string {
	var texts []string
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		evs, _ := f.srv.peersSvc.Poll(peerID)
		for _, e := range evs {
			texts = append(texts, e.Text)
		}
		time.Sleep(50 * time.Millisecond)
	}
	return texts
}

func addRepo(t *testing.T, f *windowAgentFixture, name string) string {
	t.Helper()
	repo := filepath.Join(t.TempDir(), name)
	os.MkdirAll(repo, 0o755)
	os.WriteFile(filepath.Join(repo, "README.md"), []byte("The "+name+" panel."), 0o644)
	r, err := http.Post(f.base+"/v1/workspaces", "application/json",
		strings.NewReader(`{"name":"`+name+`","repoPath":"`+repo+`","createdBy":"tester","startupCommand":""}`))
	if err != nil {
		t.Fatal(err)
	}
	defer r.Body.Close()
	var ws wsResult
	json.NewDecoder(r.Body).Decode(&ws)
	return ws.ID
}

func deleteWS(t *testing.T, f *windowAgentFixture, id string) {
	t.Helper()
	req, _ := http.NewRequest("DELETE", f.base+"/v1/workspaces/"+id, nil)
	if r, err := http.DefaultClient.Do(req); err != nil || r.StatusCode >= 300 {
		t.Fatalf("delete: %v %v", r, err)
	}
}

func containing(texts []string, want string) []string {
	var out []string
	for _, x := range texts {
		if strings.Contains(x, want) {
			out = append(out, x)
		}
	}
	return out
}

// A repo joining a window is announced to the sessions already there, with
// what it owns and how to hand it work; being deleted is announced as
// leaving. An agent's first start in a window is announced with its
// description. (A session never hears about itself: TestNotifyGroup.)
func TestWindowNotices(t *testing.T) {
	noticeOwnsPoll = 20 * time.Millisecond
	f := newWindowAgentFixture(t, "sleep 30;:")
	f.srv.SetOwns(repoowns.New(f.st, func(context.Context, string, int) (string, error) { return "admin panel, users", nil }), f.st)
	obs := noticeObserver(t, f)

	admin := addRepo(t, f, "admin")
	f.put(t, "/v1/workspaces/"+admin+"/group", `{"group":"Chart Labs"}`, 204)
	joined := containing(noticesFor(f, obs, 2*time.Second), "admin joined this window")
	if len(joined) != 1 || !strings.Contains(joined[0], "owns admin panel, users") ||
		!strings.Contains(joined[0], "to_name=admin") || !strings.Contains(joined[0], "No reply needed") {
		t.Fatalf("join notices = %q", joined)
	}
	deleteWS(t, f, admin)
	if left := containing(noticesFor(f, obs, time.Second), "admin left this window"); len(left) != 1 ||
		!strings.Contains(left[0], "nobody here owns its repo now") {
		t.Fatalf("leave notices = %q", left)
	}

	// A second agent's first start in the window is announced. (A restart
	// puts it in the same window, which is no change: TestMembershipChange.)
	f.put(t, "/v1/agents/kb-writer", `{"icon":"📚","description":"Writes KB articles.","harness":"noop","instructions":"# Role\n"}`, 200)
	f.start(t, "kb-writer", "")
	if got := containing(noticesFor(f, obs, time.Second), "kb-writer (an agent) joined this window"); len(got) != 1 ||
		!strings.Contains(got[0], "Writes KB articles") {
		t.Fatalf("agent join notices = %q", got)
	}
}

// A repo added and then removed before its owns line is ready is announced
// as leaving and never as joining: the leave is not overtaken by a join
// still waiting for its line, and that join is dropped once it is stale.
func TestWindowNotices_ALeaveIsNotOvertaken(t *testing.T) {
	noticeOwnsPoll, noticeOwnsWait = 20*time.Millisecond, time.Second
	f := newWindowAgentFixture(t, "sleep 30;:")
	release := make(chan struct{})
	f.srv.SetOwns(repoowns.New(f.st, func(context.Context, string, int) (string, error) {
		<-release
		return "admin panel", nil
	}), f.st)
	defer close(release)
	obs := noticeObserver(t, f)

	admin := addRepo(t, f, "admin")
	f.put(t, "/v1/workspaces/"+admin+"/group", `{"group":"Chart Labs"}`, 204)
	deleteWS(t, f, admin)
	texts := containing(noticesFor(f, obs, 3*time.Second), "admin ")
	if len(texts) != 1 || !strings.Contains(texts[0], "admin left this window") {
		t.Fatalf("notices = %q, want only the leave", texts)
	}
}
