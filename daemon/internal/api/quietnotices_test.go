package api

import (
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"ccmux.dev/ccmuxd/internal/agent/agenttest"
	"ccmux.dev/ccmuxd/internal/manager"
	"ccmux.dev/ccmuxd/internal/peers"
)

// The prompt hook's route: the pane's own token takes the waiting quiet
// notices once, as text; a second call finds none.
func TestPaneNotices_TakesWaitingNoticesOnce(t *testing.T) {
	f := newWindowAgentFixture(t, "sleep 8;:")
	f.srv.EnablePeers(peers.NewService(f.st, f.srv.mgr, testSecret))
	code, pane := f.start(t, "x-poster", "")
	if code != 201 {
		t.Fatalf("start = %d", code)
	}
	worker, workerTok := registerOnPane(t, f, pane.ID, "/agents/x-poster")
	sender, senderTok := registerOnPane(t, f, f.ws.Panes[0].ID, "/repo/hq")
	resp := postJSON(t, f.base+"/v1/peers/send", senderTok, map[string]any{
		"from_id": sender, "to_id": worker, "text": "fyi: brief 12 is out", "quiet": true})
	var sent peers.SendResp
	if err := json.NewDecoder(resp.Body).Decode(&sent); err != nil || !sent.OK {
		t.Fatalf("quiet send = %d %+v %v", resp.StatusCode, sent, err)
	}
	take := func(tok string) (int, string) {
		resp := postJSON(t, f.base+"/v1/panes/"+pane.ID+"/notices", tok, map[string]any{})
		var body struct {
			Text string `json:"text"`
		}
		_ = json.NewDecoder(resp.Body).Decode(&body)
		return resp.StatusCode, body.Text
	}
	if code, _ := take(senderTok); code != 401 {
		t.Errorf("another pane's token = %d, want 401", code)
	}
	if code, text := take(workerTok); code != 200 || !strings.Contains(text, "fyi: brief 12 is out") {
		t.Fatalf("take = %d %q", code, text)
	}
	if code, text := take(workerTok); code != 200 || text != "" {
		t.Fatalf("second take = %d %q, want none left", code, text)
	}
}

// A start's first prompt carries the quiet notices waiting for the pane,
// and only the mark after a launch that went through takes them away; a
// start with no prompt leaves them waiting. (The launch itself pushes the
// prompt to a daemon-chosen port no test listens on, so startAgent's two
// calls are exercised here directly.)
func TestWithQuietNotices_FoldsIntoAPromptAndMarksOnlyOnRequest(t *testing.T) {
	f := newWindowAgentFixture(t, "sleep 8;:")
	svc := peers.NewService(f.st, f.srv.mgr, testSecret)
	f.srv.EnablePeers(svc)
	code, pane := f.start(t, "x-poster", "")
	if code != 201 {
		t.Fatalf("start = %d", code)
	}
	worker, _ := registerOnPane(t, f, pane.ID, "/agents/x-poster")
	sender, senderTok := registerOnPane(t, f, f.ws.Panes[0].ID, "/repo/hq")
	postJSON(t, f.base+"/v1/peers/send", senderTok, map[string]any{
		"from_id": sender, "to_id": worker, "text": "fyi: brief 12 is out", "quiet": true})

	if got, upto := f.srv.withQuietNotices(pane.ID, ""); got != "" || upto != 0 {
		t.Fatalf("no prompt = %q %d, want nothing folded", got, upto)
	}
	got, upto := f.srv.withQuietNotices(pane.ID, "go")
	if !strings.HasPrefix(got, "go\n\n") || !strings.Contains(got, "fyi: brief 12 is out") || upto == 0 {
		t.Fatalf("prompt = %q %d", got, upto)
	}
	// A launch that failed never reaches the mark: the notice still waits.
	if again, _ := f.srv.withQuietNotices(pane.ID, "go"); !strings.Contains(again, "fyi") {
		t.Fatal("notices taken without a mark")
	}
	f.srv.markQuietNotices(pane.ID, upto)
	if again, _ := f.srv.withQuietNotices(pane.ID, "go"); again != "go" {
		t.Fatalf("after the mark = %q, want the bare prompt", again)
	}
	if got, upto := (&Server{}).withQuietNotices(pane.ID, "go"); got != "go" || upto != 0 {
		t.Fatalf("bus off = %q %d", got, upto)
	}
}

// The notices folded into a first prompt are marked only when that prompt
// reached the harness; a push that failed leaves them waiting.
func TestPushPromptLater_MarksNoticesOnlyWhenThePromptLanded(t *testing.T) {
	oc := agenttest.NewFakeOpencode()
	t.Cleanup(oc.Server.Close)
	f := newWindowAgentFixture(t, "sleep 8;:")
	svc := peers.NewService(f.st, f.srv.mgr, testSecret)
	f.srv.EnablePeers(svc)
	code, pane := f.start(t, "x-poster", "")
	if code != 201 {
		t.Fatalf("start = %d", code)
	}
	worker, _ := registerOnPane(t, f, pane.ID, "/agents/x-poster")
	sender, senderTok := registerOnPane(t, f, f.ws.Panes[0].ID, "/repo/hq")
	postJSON(t, f.base+"/v1/peers/send", senderTok, map[string]any{
		"from_id": sender, "to_id": worker, "text": "fyi: brief 12 is out", "quiet": true})
	prompt, upto := f.srv.withQuietNotices(pane.ID, "go")

	// A harness that is up but refuses the prompt: the push fails at once.
	refused := make(chan struct{}, 1)
	refuses := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/session" {
			w.Write([]byte("[]"))
			return
		}
		http.Error(w, "no", http.StatusInternalServerError)
		refused <- struct{}{}
	}))
	t.Cleanup(refuses.Close)
	refusesPort := refuses.Listener.Addr().(*net.TCPAddr).Port
	f.srv.pushPromptLater(pane.ID, manager.AgentLaunch{Name: "x-poster", Port: refusesPort, Prompt: prompt}, upto)
	select {
	case <-refused:
	case <-time.After(10 * time.Second):
		t.Fatal("the push never reached the harness")
	}
	time.Sleep(300 * time.Millisecond) // a mark, if there were one, follows at once
	if text, _ := svc.NoticesForPane(pane.ID); text == "" {
		t.Fatal("a first prompt that never landed marked its notices delivered")
	}
	f.srv.pushPromptLater(pane.ID, manager.AgentLaunch{Name: "x-poster", Port: oc.Port(), Prompt: prompt}, upto)
	waitFor(t, "notices marked after the prompt landed", func() bool {
		text, _ := svc.NoticesForPane(pane.ID)
		return text == ""
	})
}
