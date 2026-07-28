package peers

import (
	"os"
	"strings"
	"testing"
	"time"
)

// twoGroups sets up a sender in MIXED and a target named "backend" in CHARTLABS.
func twoGroups(t *testing.T) (svc *Service, sender, target string) {
	t.Helper()
	svc, hook := newTestService(t)
	hook.setGroup("pane-mine", "MIXED")
	hook.setGroup("pane-theirs", "CHARTLABS")
	sender = registerPane(svc, "pane-mine", "/w/ccmux").PeerID
	target = registerPane(svc, "pane-theirs", "/w/backend").PeerID
	return svc, sender, target
}

// The default is unchanged: reaching another project without naming it is
// refused, and the refusal now tells you how to do it deliberately.
func TestSend_CrossGroupRefusedWithoutToGroup(t *testing.T) {
	svc, sender, target := twoGroups(t)
	resp := svc.Send(SendReq{FromID: sender, ToID: target, Text: "hi"})
	if resp.OK {
		t.Fatal("a bare cross-group send must still be refused")
	}
	if !strings.Contains(resp.Error, `to_group="CHARTLABS"`) {
		t.Fatalf("error should name the group to pass, got %q", resp.Error)
	}
}

// "Check in with backend in ChartLabs" — naming the group authorizes the send.
func TestSend_CrossGroupAllowedByName(t *testing.T) {
	svc, sender, _ := twoGroups(t)
	resp := svc.Send(SendReq{FromID: sender, ToName: "backend", ToGroup: "CHARTLABS", Text: "status?"})
	if !resp.OK {
		t.Fatalf("naming the group should authorize the send, got %+v", resp)
	}
}

// A to_group that doesn't match where the peer actually lives authorizes
// nothing — the crossing must be to the group the sender actually named.
func TestSend_ToGroupMustMatchTheTarget(t *testing.T) {
	svc, sender, target := twoGroups(t)
	resp := svc.Send(SendReq{FromID: sender, ToID: target, ToGroup: "SOMEWHERE-ELSE", Text: "hi"})
	if resp.OK {
		t.Fatal("a mismatched to_group must not authorize a crossing")
	}
}

// The recipient must be able to answer with a plain to_id reply, or a
// cross-project request is a one-way street and the loop never closes.
func TestSend_CrossGroupOpensReplyPath(t *testing.T) {
	svc, sender, target := twoGroups(t)
	if resp := svc.Send(SendReq{FromID: sender, ToID: target, ToGroup: "CHARTLABS", Text: "status?"}); !resp.OK {
		t.Fatalf("outbound cross-group send failed: %+v", resp)
	}
	if resp := svc.Send(SendReq{FromID: target, ToID: sender, Text: "all good"}); !resp.OK {
		t.Fatalf("reply across the boundary should be allowed, got %+v", resp)
	}
}

// The grant is a reply window, not a permanent hole in the boundary.
func TestSend_ReplyGrantExpires(t *testing.T) {
	svc, sender, target := twoGroups(t)
	now := time.Unix(1_700_000_000, 0)
	svc.Now = func() time.Time { return now }
	if resp := svc.Send(SendReq{FromID: sender, ToID: target, ToGroup: "CHARTLABS", Text: "status?"}); !resp.OK {
		t.Fatalf("outbound cross-group send failed: %+v", resp)
	}

	now = now.Add(replyGrantTTL + time.Minute)
	if resp := svc.Send(SendReq{FromID: target, ToID: sender, Text: "late"}); resp.OK {
		t.Fatal("the reply window must not stay open forever")
	}
}

// A grant is directional: being messaged by one project does not open the whole
// boundary to unrelated peers in it.
func TestSend_ReplyGrantIsDirectional(t *testing.T) {
	svc, hook := newTestService(t)
	hook.setGroup("pane-mine", "MIXED")
	hook.setGroup("pane-other", "MIXED")
	hook.setGroup("pane-theirs", "CHARTLABS")
	sender := registerPane(svc, "pane-mine", "/w/ccmux").PeerID
	bystander := registerPane(svc, "pane-other", "/w/poly").PeerID
	target := registerPane(svc, "pane-theirs", "/w/backend").PeerID

	if resp := svc.Send(SendReq{FromID: sender, ToID: target, ToGroup: "CHARTLABS", Text: "hi"}); !resp.OK {
		t.Fatalf("outbound cross-group send failed: %+v", resp)
	}
	if resp := svc.Send(SendReq{FromID: target, ToID: bystander, Text: "hello stranger"}); resp.OK {
		t.Fatal("being messaged by one peer must not open the boundary to others")
	}
}

// Discovery half: you can look into a named group, and your own view is
// unchanged when you don't.
func TestList_GroupArgumentLooksIntoAnotherProject(t *testing.T) {
	svc, sender, target := twoGroups(t)
	if got := svc.List(sender, "project", ""); len(got) != 0 {
		t.Fatalf("own group has no other peers, got %+v", got)
	}
	got := svc.List(sender, "project", "CHARTLABS")
	if len(got) != 1 || got[0].ID != target {
		t.Fatalf("explicit group should reveal that project's peers, got %+v", got)
	}
	if got[0].Group != "CHARTLABS" {
		t.Fatalf("entry must carry the group to pass as to_group, got %q", got[0].Group)
	}
}

// The point of naming a group after the folder that holds the repos: a Claude
// started in a plain terminal inside a project lands in that project, and can
// talk to the panes there without naming a group at all. The old full path
// could never equal a window group, so such a session was marooned.
func TestFallbackGroup_MatchesTheWindowGroupName(t *testing.T) {
	cases := []struct{ gitRoot, cwd, want string }{
		{"/Users/p/Work/Coding/ChartLabs/backend", "", "ChartLabs"},
		{"", "/Users/p/Work/Coding/ChartLabs/backend", "ChartLabs"},
		{"/Users/p/Work/Coding/ccmux", "", "Coding"},
		{"", "", ""},
	}
	for _, c := range cases {
		if got := fallbackGroup(c.gitRoot, c.cwd); got != c.want {
			t.Errorf("fallbackGroup(%q,%q) = %q, want %q", c.gitRoot, c.cwd, got, c.want)
		}
	}
}

// A window someone typed as "chartlabs" and a folder named "ChartLabs" are one
// project, not two that cannot see each other. Names keep the spelling they
// were given; only the comparison ignores case.
func TestSameGroup_IgnoresCaseOnly(t *testing.T) {
	if !sameGroup("ChartLabs", "chartlabs") {
		t.Error("case must not split a group")
	}
	if !sameGroup("MIXED", "Mixed") {
		t.Error("case must not split a group")
	}
	if sameGroup("ChartLabs", "Coding") {
		t.Error("different names are different groups")
	}
	if sameGroup("", "Coding") {
		t.Error("an empty group must not match a real one")
	}
}

// End to end: a plain-terminal session in ChartLabs/backend reaches the panes
// in the ChartLabs window with no to_group, even when the window's name was
// typed in a different case.
func TestSend_PlainTerminalJoinsItsProject(t *testing.T) {
	svc, hook := newTestService(t)
	hook.setGroup("pane-be", "chartlabs") // window name, lowercased by whoever typed it
	pane := registerPane(svc, "pane-be", "/Users/p/Work/Coding/ChartLabs/backend")
	term := svc.Register(RegisterReq{PID: os.Getpid(),
		CWD:     "/Users/p/Work/Coding/ChartLabs/app",
		GitRoot: "/Users/p/Work/Coding/ChartLabs/app"})

	if term.Group != "ChartLabs" {
		t.Fatalf("terminal group = %q, want ChartLabs", term.Group)
	}
	if resp := svc.Send(SendReq{FromID: term.PeerID, ToID: pane.PeerID, Text: "hi"}); !resp.OK {
		t.Fatalf("a plain terminal must reach its project with no to_group: %+v", resp)
	}
}
