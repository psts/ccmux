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

// A name running nowhere is the one miss where spawning is the right advice.
func TestSend_NameMissingEverywhereHintsSpawn(t *testing.T) {
	svc, sender, _ := twoGroups(t)
	resp := svc.Send(SendReq{FromID: sender, ToName: "nobody", Text: "hi"})
	if resp.OK || !strings.Contains(resp.Error, "spawn_if_missing=true") {
		t.Fatalf("a name running nowhere should hint spawn_if_missing, got %+v", resp)
	}
}

// A miss inside a named group must not hint a spawn: the spawn path starts the
// teammate in the sender's own window, not the group the sender named.
func TestSend_NameMissingInToGroupNoSpawnHint(t *testing.T) {
	svc, sender, _ := twoGroups(t)
	resp := svc.Send(SendReq{FromID: sender, ToName: "nobody", ToGroup: "CHARTLABS", Text: "hi"})
	if resp.OK || strings.Contains(resp.Error, "spawn_if_missing") ||
		!strings.Contains(resp.Error, `"CHARTLABS"`) {
		t.Fatalf("a miss in a named group should name it and not hint a spawn, got %+v", resp)
	}
}

// A departed session elsewhere is not "running": the miss hints a spawn, not a
// to_group that would only reach a dead mailbox.
func TestSend_NameDepartedElsewhereHintsSpawn(t *testing.T) {
	svc, sender, target := twoGroups(t)
	svc.Unregister(target)
	resp := svc.Send(SendReq{FromID: sender, ToName: "backend", Text: "hi"})
	if resp.OK || !strings.Contains(resp.Error, "spawn_if_missing=true") ||
		strings.Contains(resp.Error, "to_group") {
		t.Fatalf("a departed peer elsewhere should hint spawn, got %+v", resp)
	}
}

// An agent joins the caller's project when contacted, so one running in
// another project still gets the spawn hint, not a to_group into that project.
func TestSend_AgentElsewhereHintsSpawn(t *testing.T) {
	svc, sender, _ := twoGroups(t)
	svc.IsAgent = func(name string) (bool, error) { return name == "backend", nil }
	resp := svc.Send(SendReq{FromID: sender, ToName: "backend", Text: "hi"})
	if resp.OK || !strings.Contains(resp.Error, "spawn_if_missing=true") ||
		strings.Contains(resp.Error, "to_group") {
		t.Fatalf("an agent elsewhere should hint spawn here, got %+v", resp)
	}
}

// An agent missing from a NAMED group gets no spawn hint: the spawn would start
// it in the sender's group, so the message would land in the wrong project.
func TestSend_AgentMissingInToGroupNoSpawnHint(t *testing.T) {
	svc, sender, _ := twoGroups(t)
	svc.IsAgent = func(name string) (bool, error) { return name == "x-poster", nil }
	resp := svc.Send(SendReq{FromID: sender, ToName: "x-poster", ToGroup: "CHARTLABS", Text: "hi"})
	if resp.OK || strings.Contains(resp.Error, "spawn_if_missing") ||
		!strings.Contains(resp.Error, `"CHARTLABS"`) {
		t.Fatalf("an agent missing from a named group should not hint a spawn, got %+v", resp)
	}
}

// A name running in several other groups lists them all, in a stable order,
// instead of naming whichever the map visits first.
func TestSend_NameInSeveralGroupsListsAll(t *testing.T) {
	svc, hook := newTestService(t)
	hook.setGroup("pane-mine", "MIXED")
	hook.setGroup("pane-theirs", "CHARTLABS")
	hook.setGroup("pane-third", "OTHER")
	sender := registerPane(svc, "pane-mine", "/w/ccmux").PeerID
	registerPane(svc, "pane-theirs", "/w/backend")
	registerPane(svc, "pane-third", "/x/backend")
	for i := 0; i < 5; i++ {
		resp := svc.Send(SendReq{FromID: sender, ToName: "backend", Text: "hi"})
		if resp.OK || !strings.Contains(resp.Error, `"CHARTLABS", "OTHER"`) {
			t.Fatalf("a name in two groups should list both in order, got %+v", resp)
		}
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
