package api

import (
	"bufio"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"ccmux.dev/ccmuxd/internal/hooktrace"
	"ccmux.dev/ccmuxd/internal/model"
)

func traceTo(t *testing.T) func() []hooktrace.Line {
	t.Helper()
	p := filepath.Join(t.TempDir(), "trace.jsonl")
	old := hooktrace.DefaultPath()
	hooktrace.SetPath(p)
	t.Cleanup(func() { hooktrace.SetPath(old) })

	return func() []hooktrace.Line {
		t.Helper()
		f, err := os.Open(p)
		if err != nil {
			return nil
		}
		defer f.Close()
		var out []hooktrace.Line
		sc := bufio.NewScanner(f)
		for sc.Scan() {
			var l hooktrace.Line
			if err := json.Unmarshal(sc.Bytes(), &l); err != nil {
				t.Fatalf("trace line is not JSON: %v", err)
			}
			out = append(out, l)
		}
		return out
	}
}

// The whole point of tracing the push path: when a lens is focused under one
// login and the subscription was stored under another, suppression silently
// fails and a phone buzzes at someone sitting at their screen. The "sent" line
// has to name who WAS focused, so the reader can see the two strings that didn't
// match instead of concluding nobody was there.
func TestNotifierTrace_SentLineNamesTheFocusedLogins(t *testing.T) {
	read := traceTo(t)
	store := &fakeStore{subs: []*model.PushSubscription{
		{ID: "phone", Login: "dev@example.com", Address: `{"endpoint":"e-phone"}`},
	}}
	// Same human, two identity paths: the Mac's lens joined over loopback under a
	// self-declared name, the phone subscribed over the tailnet under its verified
	// login. Plain map equality never matches them.
	focus := fakeFocus{"Dev Eloper": true}

	newNotifier(&fakeSender{}, store, focus).onAttention(context.Background(), "ws1", model.AttentionNeedsInput)

	sent := linesWith(read(), "sent")
	if len(sent) != 1 {
		t.Fatalf("want 1 sent line, got %d", len(sent))
	}
	if sent[0].Login != "dev@example.com" {
		t.Errorf("sent line login = %q, want the subscription's login", sent[0].Login)
	}
	if !strings.Contains(sent[0].Detail, "Dev Eloper") {
		t.Errorf("sent line detail = %q, want it to name the focused login that failed to suppress", sent[0].Detail)
	}
	if sent[0].WorkspaceID != "ws1" || sent[0].Attention != string(model.AttentionNeedsInput) {
		t.Errorf("sent line lost its correlation fields: %+v", sent[0])
	}
}

func TestNotifierTrace_SuppressedLineNamesTheMatch(t *testing.T) {
	read := traceTo(t)
	store := &fakeStore{subs: []*model.PushSubscription{
		{ID: "phone", Login: "dev@example.com", Address: `{"endpoint":"e-phone"}`},
	}}
	focus := fakeFocus{"dev@example.com": true}

	newNotifier(&fakeSender{}, store, focus).onAttention(context.Background(), "ws1", model.AttentionNeedsInput)

	got := linesWith(read(), "suppressed")
	if len(got) != 1 {
		t.Fatalf("want 1 suppressed line, got %d", len(got))
	}
	if got[0].Suppressed != "dev@example.com" {
		t.Errorf("suppressed_by = %q, want the login that matched", got[0].Suppressed)
	}
}

// "Why didn't my phone buzz" needs an answer even when nothing was sent, so the
// silent early returns are traced too.
func TestNotifierTrace_SilentBranchesGiveAReason(t *testing.T) {
	cases := []struct {
		name       string
		att        model.Attention
		subs       []*model.PushSubscription
		wantDetail string
	}{
		{"ambient state", model.AttentionIdle, []*model.PushSubscription{{ID: "a", Login: "x"}}, "ambient"},
		{"no subscribers", model.AttentionNeedsInput, nil, "subscribed"},
		{"done is not an alert", model.AttentionDone, []*model.PushSubscription{{ID: "a", Login: "x"}}, "idle_prompt"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			read := traceTo(t)
			store := &fakeStore{subs: tc.subs}

			newNotifier(&fakeSender{}, store, fakeFocus{}).onAttention(context.Background(), "ws1", tc.att)

			got := linesWith(read(), "no-push")
			if len(got) != 1 {
				t.Fatalf("want 1 no-push line, got %d", len(got))
			}
			if !strings.Contains(got[0].Detail, tc.wantDetail) {
				t.Errorf("detail = %q, want it to mention %q", got[0].Detail, tc.wantDetail)
			}
		})
	}
}

func TestNotifierTrace_PrunedSubscriptionIsRecorded(t *testing.T) {
	read := traceTo(t)
	sender := &fakeSender{status: map[string]int{`{"endpoint":"gone"}`: 410}}
	store := &fakeStore{subs: []*model.PushSubscription{
		{ID: "gone", Login: "x", Address: `{"endpoint":"gone"}`},
	}}

	newNotifier(sender, store, fakeFocus{}).onAttention(context.Background(), "ws1", model.AttentionNeedsInput)

	if got := linesWith(read(), "pruned"); len(got) != 1 {
		t.Fatalf("want 1 pruned line, got %d", len(got))
	}
}

func TestFocusedLogins_ReadsAsAList(t *testing.T) {
	if got := focusedLogins(nil); got != "(nobody)" {
		t.Errorf("empty set rendered %q, want an explicit (nobody)", got)
	}
	// Sorted, so two runs of the same state produce the same line.
	if got := focusedLogins(map[string]bool{"zoe": true, "amy": true}); got != "amy, zoe" {
		t.Errorf("rendered %q, want a sorted comma list", got)
	}
}

func linesWith(lines []hooktrace.Line, decision string) []hooktrace.Line {
	var out []hooktrace.Line
	for _, l := range lines {
		if l.Decision == decision {
			out = append(out, l)
		}
	}
	return out
}
