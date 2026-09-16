package api

import (
	"encoding/json"
	"net/http/httptest"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"ccmux.dev/ccmuxd/internal/agent/agenttest"
	"ccmux.dev/ccmuxd/internal/peers"
)

func TestAgentAsk_ValidationAndUnknownPane(t *testing.T) {
	f := newWindowAgentFixture(t, "sleep 1;:")
	f.srv.EnablePeers(peers.NewService(f.st, f.srv.mgr, testSecret))
	post := func(remote, token, body string) (int, string) {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest("POST", "/v1/panes/p-unknown/agent-ask", strings.NewReader(body))
		req.RemoteAddr = remote
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		f.srv.Handler().ServeHTTP(rec, req)
		return rec.Code, rec.Body.String()
	}
	tok := peers.TokenForPane(testSecret, "p-unknown")
	if code, _ := post("10.0.0.9:1234", tok, `{"kind":"permission","id":"abcde"}`); code != 403 {
		t.Errorf("non-loopback = %d, want 403", code)
	}
	// Loopback is not enough: the pane's own token, not another pane's.
	for bad, want := range map[string]string{"": "required", peers.TokenForPane(testSecret, "p-other"): "invalid", peers.PanelessToken(testSecret): "invalid"} {
		if code, msg := post("127.0.0.1:1234", bad, `{"kind":"permission","id":"abcde"}`); code != 401 || !strings.Contains(msg, want) {
			t.Errorf("token %q = %d %s, want 401 saying %q", bad, code, msg, want)
		}
	}
	for _, body := range []string{`{"kind":"permission","id":"abcdl"}`, `{"kind":"permission","id":"per_1"}`, `{"kind":"dialog","id":"abcde"}`, `{"id":"abcde"}`} {
		if code, _ := post("127.0.0.1:1234", tok, body); code != 400 {
			t.Errorf("%s = %d, want 400", body, code)
		}
	}
	if code, msg := post("127.0.0.1:1234", tok, `{"kind":"question","id":"abcde","text":"Pick one."}`); code != 404 || !strings.Contains(msg, "no peer") {
		t.Errorf("no peer on the pane = %d %s, want 404", code, msg)
	}
}

// The whole loop on a real agent pane whose chat server is the fake: the
// sidecar's card reaches the peer that delegated, that peer's "yes <id>"
// or "answer <id> <text>" lands on the fake's card, and a card the chat
// already answered (not pending on the server) is left alone.
func TestAgentAsk_RelaysToDelegatorAndAnswersTheCard(t *testing.T) {
	oc := agenttest.NewFakeOpencode()
	t.Cleanup(oc.Server.Close)
	f := newWindowAgentFixture(t, "sleep 8;: --port "+strconv.Itoa(oc.Port()))
	f.srv.EnablePeers(peers.NewService(f.st, f.srv.mgr, testSecret))
	code, pane := f.start(t, "x-poster", "")
	if code != 201 {
		t.Fatalf("start = %d", code)
	}
	worker, workerTok := registerOnPane(t, f, pane.ID, "/agents/x-poster")
	delegator, delegatorTok := registerOnPane(t, f, f.ws.Panes[0].ID, "/repo/hq")
	if resp := postJSON(t, f.base+"/v1/peers/send", delegatorTok, map[string]any{"from_id": delegator, "to_id": worker, "text": "please post it"}); resp.StatusCode != 200 {
		t.Fatalf("delegation send = %d", resp.StatusCode)
	}
	postJSON(t, f.base+"/v1/peers/poll", workerTok, map[string]any{"peer_id": worker})

	ask := func(body string) (int, string) {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest("POST", "/v1/panes/"+pane.ID+"/agent-ask", strings.NewReader(body))
		req.RemoteAddr = "127.0.0.1:1234"
		req.Header.Set("Authorization", "Bearer "+workerTok)
		f.srv.Handler().ServeHTTP(rec, req)
		return rec.Code, rec.Body.String()
	}
	inbox := func() []string {
		resp := postJSON(t, f.base+"/v1/peers/poll", delegatorTok, map[string]any{"peer_id": delegator})
		var got struct {
			Events []struct{ Text string } `json:"events"`
		}
		json.NewDecoder(resp.Body).Decode(&got)
		texts := []string{}
		for _, e := range got.Events {
			texts = append(texts, e.Text)
		}
		return texts
	}
	send := func(text string) {
		if resp := postJSON(t, f.base+"/v1/peers/send", delegatorTok, map[string]any{"from_id": delegator, "to_id": worker, "text": text}); resp.StatusCode != 200 {
			t.Fatalf("send %q = %d", text, resp.StatusCode)
		}
	}

	// A permission card: relayed with the frozen wording, answered "once".
	if code, body := ask(`{"kind":"permission","id":"abcde","tool":"Bash","description":"post the thread","preview":"printf hi"}`); code != 200 || !strings.Contains(body, `"relayed_to":1`) {
		t.Fatalf("permission ask = %d %s", code, body)
	}
	texts := inbox()
	if len(texts) != 1 || !strings.HasPrefix(texts[0], "[claude-peers permission relay]") || !strings.Contains(texts[0], `"yes abcde"`) || !strings.Contains(texts[0], "printf hi") {
		t.Fatalf("delegator inbox = %q", texts)
	}
	oc.AskPermission("abcde")
	send("yes abcde")
	waitFor(t, "permission reply on the card", func() bool {
		_, _, replies := oc.Recorded()
		return replies["abcde"] == "once"
	})

	// A delegator's "no" rejects the card (and only that card: per_1 stays).
	if code, _ := ask(`{"kind":"permission","id":"defgh","tool":"Write","description":"rewrite the brief","preview":"{\"file_path\":\"AGENTS.md\"}"}`); code != 200 {
		t.Fatalf("second permission ask = %d", code)
	}
	inbox()
	oc.AskPermission("defgh")
	send("no defgh")
	waitFor(t, "permission rejection on the card", func() bool {
		_, _, replies := oc.Recorded()
		return replies["defgh"] == "reject"
	})
	if _, _, replies := oc.Recorded(); replies["abcde"] != "once" || replies["per_1"] != "" {
		t.Errorf("the rejection must touch only its own card: %v", replies)
	}

	// A question card: its own wording; the one answer lands under both questions.
	if code, body := ask(`{"kind":"question","id":"bcdef","text":"Tone: Which tone?\n  - Warm\n  - Dry"}`); code != 200 || !strings.Contains(body, `"relayed_to":1`) {
		t.Fatalf("question ask = %d %s", code, body)
	}
	texts = inbox()
	if len(texts) != 1 || !strings.HasPrefix(texts[0], "[claude-peers question relay]") || !strings.Contains(texts[0], "  - Dry") || !strings.Contains(texts[0], `"answer bcdef <your answer>"`) {
		t.Fatalf("delegator inbox = %q", texts)
	}
	oc.AskQuestion("bcdef", 2)
	send("answer bcdef Dry, and keep it short")
	waitFor(t, "question answer on the card", func() bool {
		return reflect.DeepEqual(oc.Answers()["bcdef"], [][]string{{"Dry, and keep it short"}, {"Dry, and keep it short"}})
	})

	// A card the chat answered first is not pending on the server any more:
	// the bus verdict is dropped, nothing is replied under that id.
	if code, _ := ask(`{"kind":"permission","id":"cdefg","tool":"Write","description":"x","preview":"{}"}`); code != 200 {
		t.Fatalf("stale ask = %d", code)
	}
	inbox()
	send("no cdefg")
	time.Sleep(300 * time.Millisecond)
	if _, _, replies := oc.Recorded(); replies["cdefg"] != "" || replies["per_1"] != "" {
		t.Errorf("a card not pending must not be answered: %v", replies)
	}
}

// registerOnPane registers a bus peer on paneID with cwd (its name is the
// folder's) and returns its id and token.
func registerOnPane(t *testing.T, f *windowAgentFixture, paneID, cwd string) (string, string) {
	t.Helper()
	tok := peers.TokenForPane(testSecret, paneID)
	nextFakePID++
	resp := postJSON(t, f.base+"/v1/peers/register", tok, map[string]any{"pane_id": paneID, "pid": nextFakePID, "cwd": cwd, "git_root": cwd})
	var reg struct {
		PeerID string `json:"peer_id"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&reg); err != nil || reg.PeerID == "" {
		t.Fatalf("register on %s: %d %v", paneID, resp.StatusCode, err)
	}
	return reg.PeerID, tok
}

func waitFor(t *testing.T, what string, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !ok() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(20 * time.Millisecond)
	}
}
