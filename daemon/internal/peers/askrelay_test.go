package peers

import (
	"os"
	"strings"
	"testing"
	"time"

	"ccmux.dev/ccmuxd/internal/model"
)

// A question card relays like a permission dialog, with its own wording and
// reply form. "answer <id> <text>" becomes a question_answer event for the
// worker (the answer re-parsed from the raw text on the wire); the first
// answer wins; an id nobody asked about stays a normal message.
func TestQuestionRelay_AnswerRoutedAndFirstWins(t *testing.T) {
	svc, hook := newTestService(t)
	hook.groups["pane-w"] = "G"
	hook.groups["pane-d"] = "G"
	worker := registerPane(svc, "pane-w", "/w/worker").PeerID
	delegator := registerPane(svc, "pane-d", "/w/delegator").PeerID
	svc.Send(SendReq{FromID: delegator, ToID: worker, Text: "please do X"})
	svc.Poll(worker)

	relayed, err := svc.QuestionRequest(worker, "abcde", "Demo: Pick one.\n  - Tea: calm\n  - Coffee: alert")
	if err != nil || relayed != 1 {
		t.Fatalf("relayed = %d (%v), want 1", relayed, err)
	}
	evs, _ := svc.Poll(delegator)
	if len(evs) != 1 || !strings.HasPrefix(evs[0].Text, "[claude-peers question relay]") {
		t.Fatalf("delegator inbox = %+v, want the question relay", evs)
	}
	for _, want := range []string{"- Coffee: alert", `"answer abcde <your answer>"`, `delegated this work to "worker"`} {
		if !strings.Contains(evs[0].Text, want) {
			t.Errorf("relay text lacks %q:\n%s", want, evs[0].Text)
		}
	}

	// A multi-line answer, with odd case and spacing around the id.
	if resp := svc.Send(SendReq{FromID: delegator, ToID: worker, Text: "Answer  ABCDE  Tea, please.\nThe second line too. "}); !resp.OK {
		t.Fatalf("answer send failed: %+v", resp)
	}
	evs, _ = svc.Poll(worker)
	if len(evs) != 1 || evs[0].Kind != model.PeerEventAnswer || evs[0].RequestID != "abcde" {
		t.Fatalf("worker inbox = %+v, want one answer event", evs)
	}
	if got := AnswerText(evs[0].Text); got != "Tea, please.\nThe second line too." {
		t.Errorf("answer text = %q", got)
	}
	frame, ok := WireFrame(evs[0]).(wireAnswer)
	if !ok || frame.Type != "question_answer" || frame.Answer != "Tea, please.\nThe second line too." || frame.FromID != delegator {
		t.Errorf("wire frame = %+v", WireFrame(evs[0]))
	}

	// Second answer for the same id: dropped silently (first wins).
	if resp := svc.Send(SendReq{FromID: delegator, ToID: worker, Text: "answer abcde Coffee"}); !resp.OK {
		t.Fatalf("late answer: %+v", resp)
	}
	if evs, _ = svc.Poll(worker); len(evs) != 0 {
		t.Fatalf("late answer leaked through: %+v", evs)
	}

	// No outstanding ask under that id, or no text after it: a normal message.
	for _, text := range []string{"answer qwert Tea", "answer abcde", "answers abcde Tea"} {
		svc.Send(SendReq{FromID: delegator, ToID: worker, Text: text})
		evs, _ = svc.Poll(worker)
		if len(evs) != 1 || evs[0].Kind != model.PeerEventMessage || evs[0].Text != text {
			t.Fatalf("%q = %+v, want a plain message", text, evs)
		}
	}
	if AnswerText("yes abcde") != "" || AnswerText("answer abcde") != "" {
		t.Error("AnswerText must be empty for a non-answer")
	}
}

// A verdict or answer for a worker on a pane also reaches the pane's chat
// server through ReplyToPane; a pane-less worker's does not, and a service
// without the hook is unchanged.
func TestAskReply_ReachesPaneHook(t *testing.T) {
	svc, hook := newTestService(t)
	hook.groups["pane-w"] = "G"
	hook.groups["pane-d"] = "G"
	worker := registerPane(svc, "pane-w", "/w/worker").PeerID
	delegator := registerPane(svc, "pane-d", "/w/delegator").PeerID
	// A pane-less pair (both in the directory-fallback group, so they can
	// reach each other): the verdict is delivered, the hook is not called.
	roaming := registerPaneless(svc, "/w/worker").PeerID
	// A second pane-less peer needs its own live pid: one pid is one peer.
	roamingDelegator := svc.Register(RegisterReq{PID: os.Getppid(), CWD: "/w/delegator", GitRoot: "/w/delegator"}).PeerID
	got := make(chan PaneReply, 4)
	svc.ReplyToPane = func(paneID string, reply PaneReply) error {
		if paneID != "pane-w" {
			t.Errorf("reply went to pane %q", paneID)
		}
		got <- reply
		return nil
	}
	for _, pair := range [][2]string{{delegator, worker}, {roamingDelegator, roaming}} {
		if resp := svc.Send(SendReq{FromID: pair[0], ToID: pair[1], Text: "please do X"}); !resp.OK {
			t.Fatalf("delegation send: %+v", resp)
		}
		svc.Poll(pair[1])
	}

	svc.PermissionRequest(worker, "abcde", "Bash", "x", "{}")
	svc.QuestionRequest(worker, "bcdef", "Pick one.")
	svc.PermissionRequest(roaming, "cdefg", "Bash", "x", "{}")
	svc.Send(SendReq{FromID: delegator, ToID: worker, Text: "no abcde"})
	svc.Send(SendReq{FromID: delegator, ToID: worker, Text: "answer bcdef go with tea"})
	svc.Send(SendReq{FromID: roamingDelegator, ToID: roaming, Text: "yes cdefg"})

	want := map[string]PaneReply{
		"abcde": {RequestID: "abcde", Behavior: "deny"},
		"bcdef": {RequestID: "bcdef", Answer: "go with tea"},
	}
	for i := 0; i < 2; i++ {
		select {
		case r := <-got:
			if want[r.RequestID] != r {
				t.Errorf("hook got %+v, want %+v", r, want[r.RequestID])
			}
			delete(want, r.RequestID)
		case <-time.After(3 * time.Second):
			t.Fatalf("hook never got %v", want)
		}
	}
	select {
	case r := <-got:
		t.Errorf("a pane-less worker's verdict reached the hook: %+v", r)
	case <-time.After(100 * time.Millisecond):
	}
	// The events themselves reached every worker regardless of the hook.
	if evs, _ := svc.Poll(roaming); len(evs) != 1 || evs[0].Kind != model.PeerEventVerdict {
		t.Errorf("pane-less worker inbox = %+v", evs)
	}
}

// PeerForPane names the present peer on a pane and nobody else.
func TestPeerForPane(t *testing.T) {
	svc, hook := newTestService(t)
	hook.groups["pane-w"] = "G"
	worker := registerPane(svc, "pane-w", "/w/worker").PeerID
	registerPaneless(svc, "/w/worker")
	if got := svc.PeerForPane("pane-w"); got != worker {
		t.Errorf("PeerForPane = %q, want %q", got, worker)
	}
	if got := svc.PeerForPane("pane-x"); got != "" {
		t.Errorf("unknown pane = %q", got)
	}
	svc.Unregister(worker)
	if got := svc.PeerForPane("pane-w"); got != "" {
		t.Errorf("a departed peer is not on the pane any more: %q", got)
	}
}

// A reply in the other verb (yes/no for a question, answer for a permission)
// is a normal message and leaves the ask open: the right verb still lands
// afterwards, and the worker never sees an event of the wrong kind.
func TestAskReply_WrongVerbLeavesTheAskOpen(t *testing.T) {
	svc, hook := newTestService(t)
	hook.groups["pane-w"] = "G"
	hook.groups["pane-d"] = "G"
	worker := registerPane(svc, "pane-w", "/w/worker").PeerID
	delegator := registerPane(svc, "pane-d", "/w/delegator").PeerID
	svc.Send(SendReq{FromID: delegator, ToID: worker, Text: "please do X"})
	svc.Poll(worker)
	svc.QuestionRequest(worker, "abcde", "Pick one.")
	svc.PermissionRequest(worker, "bcdef", "Bash", "x", "{}")

	for _, wrong := range []string{"yes abcde", "answer bcdef yes go ahead"} {
		if resp := svc.Send(SendReq{FromID: delegator, ToID: worker, Text: wrong}); !resp.OK {
			t.Fatalf("%q: %+v", wrong, resp)
		}
		evs, _ := svc.Poll(worker)
		if len(evs) != 1 || evs[0].Kind != model.PeerEventMessage || evs[0].Text != wrong {
			t.Fatalf("%q = %+v, want a plain message", wrong, evs)
		}
	}
	svc.Send(SendReq{FromID: delegator, ToID: worker, Text: "answer abcde Tea"})
	svc.Send(SendReq{FromID: delegator, ToID: worker, Text: "no bcdef"})
	evs, _ := svc.Poll(worker)
	if len(evs) != 2 || evs[0].Kind != model.PeerEventAnswer || evs[0].RequestID != "abcde" ||
		evs[1].Kind != model.PeerEventVerdict || evs[1].RequestID != "bcdef" || evs[1].Behavior != "deny" {
		t.Fatalf("the right verbs after the wrong ones = %+v", evs)
	}
}
