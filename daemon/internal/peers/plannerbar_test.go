package peers

import (
	"strings"
	"testing"
)

// plannerOn puts app's planner on the bus, on runner pane "plana". It is in
// no helper routing here (ThreadRoute is nil): what these tests hold is the
// bus's own bar, for the names the routing lets through.
func plannerOn(b *mainBus) string {
	b.hook.groups["plana"] = "SHOP"
	b.setRunner("plana", RunnerInfo{Name: "app-s-99aa88bb", Repo: "/r/app", Planner: true})
	b.svc.EnsureRunnerPeer("plana", "/r/app")
	return derivedID("plana")
}

// A planner's by-name message never goes on to a repo's main session, and
// never starts one, by any door: a send, with or without spawn_if_missing,
// a delegation, a quiet notice. Not its own repo's, not another repo's of
// its window (the moment before its helper routing is there), not another
// project's.
func TestAPlannerNeverReachesOrStartsAMainSessionByName(t *testing.T) {
	doors := map[string]func(b *mainBus, from, name, group string) (bool, string){
		"send": func(b *mainBus, from, name, group string) (bool, string) {
			r := b.svc.Send(SendReq{FromID: from, ToName: name, ToGroup: group, Text: "q"})
			return r.OK, r.Error
		},
		"send, spawn_if_missing": func(b *mainBus, from, name, group string) (bool, string) {
			r := b.svc.Send(SendReq{FromID: from, ToName: name, ToGroup: group, Text: "q", SpawnIfMissing: true})
			return r.OK, r.Error
		},
		"delegate, spawn_if_missing": func(b *mainBus, from, name, group string) (bool, string) {
			r := b.svc.Delegate(DelegateReq{FromID: from, ToName: name, ToGroup: group, Text: "q", SpawnIfMissing: true})
			return r.OK, r.Error
		},
		"quiet": func(b *mainBus, from, name, group string) (bool, string) {
			r := b.svc.Send(SendReq{FromID: from, ToName: name, ToGroup: group, Text: "q", Quiet: true})
			return r.OK, r.Error
		},
	}
	cases := map[string]struct {
		name, group string
		mains       bool // the repos have main sessions (else one would be started)
		own         bool
	}{
		"its own repo, which has a main session":        {name: "app", mains: true, own: true},
		"its own repo, which has none":                  {name: "app", own: true},
		"another repo of its window":                    {name: "backend", mains: true},
		"another repo of its window with no session":    {name: "backend"},
		"a repo of another project":                     {name: "site", group: "OTHER", mains: true},
		"a repo of another project that has no session": {name: "site", group: "OTHER"},
	}
	for door, send := range doors {
		for what, c := range cases {
			b := newMainBus(t)
			b.hook.groups["mains"] = "OTHER"
			b.hook.closed["OTHER\x00site"] = "ws-site:/r/site"
			if c.mains {
				b.haveMain("maina", "app", true)
				b.haveMain("mainb", "backend", true)
				b.haveMain("mains", "site", true)
			}
			ok, why := send(b, plannerOn(b), c.name, c.group)
			if ok || !strings.Contains(why, "a planner never messages it") {
				t.Errorf("%s to %s: ok %v, %q; want it refused as a planner's", door, what, ok, why)
			}
			if c.own != strings.Contains(why, "is your own repo") {
				t.Errorf("%s to %s: %q; own repo said: want %v", door, what, why, c.own)
			}
			b.svc.ReapOnce()
			settle()
			if got := append(append(b.to("maina"), b.to("mainb")...), b.to("mains")...); len(got) != 0 || len(b.starts()) != 0 || b.hook.spawnCount() != 0 {
				t.Errorf("%s to %s: main sessions got %v, starts %v, %d terminal spawns; want nothing", door, what, got, b.starts(), b.hook.spawnCount())
			}
			for _, pane := range []string{"maina", "mainb", "mains"} {
				if pending, _ := b.svc.st.PendingNotices(derivedID(pane)); len(pending) != 0 {
					t.Errorf("%s to %s: a notice waits for %s: %v", door, what, pane, pending)
				}
			}
		}
	}
}

// What the bar leaves alone: a present peer called exactly the name (a
// session somebody started under it), a name that is no repo, and every
// sender that is no planner.
func TestThePlannerBarLeavesEverythingElseAsItWas(t *testing.T) {
	b := newMainBus(t)
	b.haveMain("mainb", "backend", true)
	planner := plannerOn(b)
	term := registerPane(b.svc, "tmuxb", "/r/backend").PeerID
	if resp := b.svc.Send(SendReq{FromID: planner, ToName: "backend", Text: "hi"}); !resp.OK {
		t.Fatalf("to a peer called exactly backend: %+v", resp)
	}
	if got := b.mail(t, term); len(got) != 1 {
		t.Fatalf("the terminal session's mail = %v, want the planner's message", got)
	}
	if resp := b.svc.Send(SendReq{FromID: planner, ToName: "nobody", Text: "hi"}); resp.OK || strings.Contains(resp.Error, "planner") {
		t.Fatalf("a name that is no repo: %+v, want the usual miss", resp)
	}
	// Its own repo is refused even with a session called exactly that there
	// (the rig's sender is one, on tmuxa): that is a person's conversation.
	if resp := b.svc.Send(SendReq{FromID: planner, ToName: "app", Text: "hi"}); resp.OK || !strings.Contains(resp.Error, "is your own repo") {
		t.Fatalf("its own repo, with a terminal session called app there: %+v", resp)
	}
	if got := b.mail(t, b.app); len(got) != 0 {
		t.Fatalf("the terminal session in the planner's own repo got %v", got)
	}
	b.svc.Unregister(term)
	if resp := b.svc.Send(SendReq{FromID: b.app, ToName: "backend", Text: "from a session that is no planner"}); !resp.OK {
		t.Fatalf("any other sender still reaches the main session: %+v", resp)
	}
	eventually(t, "the message in the main session's inbox", func() bool { return len(b.to("mainb")) == 1 })
}
