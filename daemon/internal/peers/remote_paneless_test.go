package peers

import (
	"testing"
)

// registerRemotePaneless registers a pane-less session arriving from a member
// host's address, the way the HTTP layer does (origin from the socket, never
// from the body).
func registerRemotePaneless(svc *Service, cwd string, pid int, ip string) RegisterResp {
	return svc.RegisterFrom(RegisterReq{PID: pid, CWD: cwd, GitRoot: cwd}, ip)
}

// hostByIP is a stand-in for the hub registry's discovery map.
func hostByIP(m map[string]string) func(string) (string, bool) {
	return func(ip string) (string, bool) {
		h, ok := m[ip]
		return h, ok
	}
}

// A pane-less session on a member host is labelled with that host, so its
// listing distinguishes it from a same-named session elsewhere — and, more
// importantly, so every pid rule below knows the pid is not local.
func TestRemotePaneless_StampsOwningHost(t *testing.T) {
	svc, _ := newTestService(t)
	svc.EnableFederation(nil, nil, hostByIP(map[string]string{"100.80.17.39": "patric-ccmux"}))

	resp := registerRemotePaneless(svc, "/Users/p/Work/ChartLabs/admin", 424242, "100.80.17.39")

	svc.mu.Lock()
	p := svc.peers[resp.PeerID]
	svc.mu.Unlock()
	if p == nil {
		t.Fatal("peer not registered")
	}
	if p.Host != "patric-ccmux" {
		t.Errorf("Host = %q, want patric-ccmux", p.Host)
	}
	// The group still comes from the folder holding the repo, which is what puts
	// a plain-terminal session in the same project as the panes around it.
	if resp.Group != "ChartLabs" {
		t.Errorf("group = %q, want ChartLabs", resp.Group)
	}
}

// A connection from an address the hub has NOT discovered is not labelled. It is
// a local caller (loopback) or an unknown node, and inventing a host for it
// would switch off the pid rules that are correct for a local peer.
func TestRemotePaneless_UnknownAddressIsNotLabelled(t *testing.T) {
	svc, _ := newTestService(t)
	svc.EnableFederation(nil, nil, hostByIP(map[string]string{"100.80.17.39": "patric-ccmux"}))

	resp := registerRemotePaneless(svc, "/srv/x/backend", 424243, "127.0.0.1")

	svc.mu.Lock()
	p := svc.peers[resp.PeerID]
	svc.mu.Unlock()
	if p.Host != "" {
		t.Errorf("Host = %q, want empty for an undiscovered address", p.Host)
	}
}

// The reaper's kill(0) indexes THIS machine's process table. For a peer whose
// process lives on another host that is not a liveness test, it is a coin flip
// against an unrelated local process — here, a pid that is certainly not running
// locally, which would erase a perfectly healthy remote session on the next sweep.
func TestRemotePaneless_SurvivesTheReaper(t *testing.T) {
	svc, _ := newTestService(t)
	svc.EnableFederation(nil, nil, hostByIP(map[string]string{"100.80.17.39": "patric-ccmux"}))

	// 0x7FFFFFFE: within pid_t, past any live pid on the test machine.
	resp := registerRemotePaneless(svc, "/Users/p/Work/ChartLabs/hq", 0x7FFFFFFE, "100.80.17.39")
	svc.ReapOnce()

	svc.mu.Lock()
	_, alive := svc.peers[resp.PeerID]
	svc.mu.Unlock()
	if !alive {
		t.Fatal("a live remote session was reaped because its pid does not exist HERE")
	}
}

// The mirror image: a LOCAL pane-less peer whose process is gone must still be
// reaped. The remote carve-out must not disable the rule it is carved out of.
func TestLocalPaneless_StillReapedWhenProcessIsGone(t *testing.T) {
	svc, _ := newTestService(t)

	resp := svc.Register(RegisterReq{PID: 0x7FFFFFFE, CWD: "/x/y", GitRoot: "/x/y"})
	svc.ReapOnce()

	svc.mu.Lock()
	_, alive := svc.peers[resp.PeerID]
	svc.mu.Unlock()
	if alive {
		t.Fatal("a local pane-less peer with a dead pid stayed registered")
	}
}

// Pids are unique per machine, not per fleet. Two member hosts each have a pid
// 1234, and the stale-record eviction reads pid equality as "the same terminal
// restarted" — across hosts that is two different people, and registering one
// silently unregistered the other.
func TestRemotePaneless_SamePidOnTwoHostsCoexist(t *testing.T) {
	svc, _ := newTestService(t)
	svc.EnableFederation(nil, nil, hostByIP(map[string]string{
		"100.80.17.39":  "patric-ccmux",
		"100.99.239.55": "sanlabs",
	}))

	first := registerRemotePaneless(svc, "/Users/p/Work/ChartLabs/admin", 1234, "100.80.17.39")
	second := registerRemotePaneless(svc, "/ext/projects/chartlabs/admin", 1234, "100.99.239.55")

	svc.mu.Lock()
	_, firstAlive := svc.peers[first.PeerID]
	_, secondAlive := svc.peers[second.PeerID]
	svc.mu.Unlock()
	if !firstAlive {
		t.Error("the first host's session was evicted by a same-pid session on ANOTHER host")
	}
	if !secondAlive {
		t.Error("the second host's session did not register")
	}
}

// Within ONE host the eviction must still fire: a pane-less MCP server that
// restarts in the same terminal re-registers with a new id and the same pid, and
// the old record has to go or it lingers forever with an uncollectable mailbox.
func TestRemotePaneless_SamePidSameHostStillEvicts(t *testing.T) {
	svc, _ := newTestService(t)
	svc.EnableFederation(nil, nil, hostByIP(map[string]string{"100.80.17.39": "patric-ccmux"}))

	first := registerRemotePaneless(svc, "/Users/p/Work/ChartLabs/admin", 1234, "100.80.17.39")
	second := registerRemotePaneless(svc, "/Users/p/Work/ChartLabs/website", 1234, "100.80.17.39")

	svc.mu.Lock()
	_, firstAlive := svc.peers[first.PeerID]
	_, secondAlive := svc.peers[second.PeerID]
	svc.mu.Unlock()
	if firstAlive {
		t.Error("the restarted session's old record survived on the same host")
	}
	if !secondAlive {
		t.Error("the new session did not register")
	}
}
