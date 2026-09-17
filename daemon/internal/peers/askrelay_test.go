package peers

import (
	"errors"
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
		"abcde": {Kind: AskPermission, RequestID: "abcde", Behavior: "deny"},
		"bcdef": {Kind: AskQuestion, RequestID: "bcdef", Answer: "go with tea"},
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

// A hand-off the chat server did not take (the sidecar was away) opens the
// ask again, so the delegator's next reply lands instead of being dropped
// as a duplicate. ErrNoPanePush (no chat server at all) is not a failure,
// and an ask the hook did answer stays answered.
func TestAskReply_FailedHandoffReopensTheAsk(t *testing.T) {
	svc, hook := newTestService(t)
	hook.groups["pane-w"] = "G"
	hook.groups["pane-d"] = "G"
	worker := registerPane(svc, "pane-w", "/w/worker").PeerID
	delegator := registerPane(svc, "pane-d", "/w/delegator").PeerID
	svc.Send(SendReq{FromID: delegator, ToID: worker, Text: "please do X"})
	svc.Poll(worker)
	svc.PermissionRequest(worker, "abcde", "Bash", "x", "{}")
	svc.PermissionRequest(worker, "bcdef", "Write", "y", "{}")
	svc.PermissionRequest(worker, "cdefg", "Edit", "z", "{}")

	calls := make(chan PaneReply, 8)
	svc.ReplyToPane = func(paneID string, reply PaneReply) error {
		calls <- reply
		switch reply.RequestID {
		case "abcde":
			return errors.New("connect: connection refused")
		case "bcdef":
			return ErrNoPanePush
		}
		return nil
	}
	next := func() PaneReply {
		select {
		case r := <-calls:
			return r
		case <-time.After(3 * time.Second):
			t.Fatal("hook not called")
			return PaneReply{}
		}
	}
	svc.Send(SendReq{FromID: delegator, ToID: worker, Text: "yes abcde"})
	svc.Send(SendReq{FromID: delegator, ToID: worker, Text: "yes bcdef"})
	svc.Send(SendReq{FromID: delegator, ToID: worker, Text: "yes cdefg"})
	for i := 0; i < 3; i++ {
		next()
	}
	waitOpen := func(rid string, want bool) {
		deadline := time.Now().Add(3 * time.Second)
		for {
			svc.mu.Lock()
			open := !svc.perms[rid].resolved
			svc.mu.Unlock()
			if open == want {
				return
			}
			if time.Now().After(deadline) {
				t.Fatalf("ask %s open=%v, want %v", rid, open, want)
			}
			time.Sleep(10 * time.Millisecond)
		}
	}
	waitOpen("abcde", true)
	waitOpen("bcdef", false)
	waitOpen("cdefg", false)

	// The retry on the failed one goes through as a fresh verdict and
	// reaches the hook again; retries on the answered ones are dropped.
	svc.Poll(worker)
	svc.Send(SendReq{FromID: delegator, ToID: worker, Text: "no abcde"})
	svc.Send(SendReq{FromID: delegator, ToID: worker, Text: "no bcdef"})
	svc.Send(SendReq{FromID: delegator, ToID: worker, Text: "no cdefg"})
	if r := next(); r.RequestID != "abcde" || r.Behavior != "deny" {
		t.Errorf("retry reached the hook as %+v", r)
	}
	select {
	case r := <-calls:
		t.Errorf("a retry on an answered ask reached the hook: %+v", r)
	case <-time.After(100 * time.Millisecond):
	}
	evs, _ := svc.Poll(worker)
	if len(evs) != 1 || evs[0].RequestID != "abcde" || evs[0].Behavior != "deny" {
		t.Errorf("worker inbox after the retries = %+v, want the one deny for abcde", evs)
	}
}

// A delegator whose only message is older than the recent-sender window
// still hears the card, on the strength of the open delegation: a long task
// raises its card long after the delegating message. Closing the task ends
// that, and a plain sender past the window is not reached either.
func TestAskRelay_OpenDelegationOutlivesRecentWindow(t *testing.T) {
	svc, hook := newTestService(t)
	hook.groups["pane-w"] = "G"
	hook.groups["pane-d"] = "G"
	hook.groups["pane-s"] = "G"
	worker := registerPane(svc, "pane-w", "/w/worker").PeerID
	delegator := registerPane(svc, "pane-d", "/w/delegator").PeerID
	sender := registerPane(svc, "pane-s", "/w/sender").PeerID
	base := time.Now()
	svc.Now = func() time.Time { return base }
	taskID := svc.Delegate(DelegateReq{FromID: delegator, ToID: worker, Text: "long job"}).TaskID
	if taskID == "" {
		t.Fatal("delegate failed")
	}
	svc.Send(SendReq{FromID: sender, ToID: worker, Text: "hi"})
	// The worker hands out more sub-tasks than any listing cap and leaves
	// them open: its own outgoing work must not crowd its delegator out.
	for i := 0; i < 25; i++ {
		svc.Now = func() time.Time { return base.Add(time.Duration(i+1) * time.Second) }
		if r := svc.Delegate(DelegateReq{FromID: worker, ToID: sender, Text: "sub"}); r.TaskID == "" {
			t.Fatalf("sub-delegate %d failed: %+v", i, r)
		}
	}
	svc.Poll(worker)
	svc.Poll(delegator)
	svc.Poll(sender)

	svc.Now = func() time.Time { return base.Add(recentSenderWindow + time.Hour) }
	relayed, err := svc.PermissionRequest(worker, "abcde", "Bash", "run it", "rm x")
	if err != nil || relayed != 1 {
		t.Fatalf("relayed = %d (%v), want 1 (the delegator only)", relayed, err)
	}
	if evs, _ := svc.Poll(delegator); len(evs) != 1 || !strings.HasPrefix(evs[0].Text, "[claude-peers permission relay]") {
		t.Fatalf("delegator inbox = %+v, want the relay", evs)
	}
	if evs, _ := svc.Poll(sender); len(evs) != 0 {
		t.Fatalf("sender past the window got %+v, want nothing", evs)
	}

	if resp := svc.UpdateTask(TaskUpdateReq{PeerID: worker, TaskID: taskID, Status: "completed", Result: "done"}); !resp.OK {
		t.Fatalf("close task: %+v", resp)
	}
	if relayed, err = svc.PermissionRequest(worker, "bcdef", "Bash", "run it", "rm y"); err != nil || relayed != 0 {
		t.Fatalf("after close relayed = %d (%v), want 0", relayed, err)
	}
}

// Inside the window the delegator is a recent sender AND an open delegator:
// it hears the card once, not twice. A second answer would be swallowed as
// a duplicate, so a doubled relay would cost the delegator a wasted turn.
func TestAskRelay_DelegatorInWindowHeardOnce(t *testing.T) {
	svc, hook := newTestService(t)
	hook.groups["pane-w"] = "G"
	hook.groups["pane-d"] = "G"
	worker := registerPane(svc, "pane-w", "/w/worker").PeerID
	delegator := registerPane(svc, "pane-d", "/w/delegator").PeerID
	if r := svc.Delegate(DelegateReq{FromID: delegator, ToID: worker, Text: "quick job"}); r.TaskID == "" {
		t.Fatalf("delegate failed: %+v", r)
	}
	svc.Poll(worker)
	svc.Poll(delegator)

	relayed, err := svc.PermissionRequest(worker, "abcde", "Bash", "run it", "ls")
	if err != nil || relayed != 1 {
		t.Fatalf("relayed = %d (%v), want exactly 1", relayed, err)
	}
	evs, _ := svc.Poll(delegator)
	if len(evs) != 1 || !strings.HasPrefix(evs[0].Text, "[claude-peers permission relay]") {
		t.Fatalf("delegator inbox = %+v, want one relay", evs)
	}
}
