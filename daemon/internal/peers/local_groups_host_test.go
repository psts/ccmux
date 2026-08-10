package peers

import (
	"os"
	"testing"
	"time"
)

// On the hub the local-pane map is the UNION of every member's, and each member
// pushes a complete replacement of its own view. Keyed flat, the second member's
// push deleted the first's entries and those sessions silently fell back to the
// dirname group.
func TestLocalPaneGroups_OneHostDoesNotClobberAnother(t *testing.T) {
	svc, _ := newTestService(t)
	svc.EnableFederation(nil, nil, func(ip string) (string, bool) {
		host, ok := map[string]string{"100.0.0.1": "mac-one", "100.0.0.2": "mac-two"}[ip]
		return host, ok
	})

	const uuidOne = "11111111-0000-0000-0000-000000000000"
	const uuidTwo = "22222222-0000-0000-0000-000000000000"
	one := svc.RegisterFrom(RegisterReq{LocalPaneID: uuidOne, PID: os.Getpid(),
		CWD: "/w/ChartLabs/backend", GitRoot: "/w/ChartLabs/backend"}, "100.0.0.1")
	two := svc.RegisterFrom(RegisterReq{LocalPaneID: uuidTwo, PID: os.Getpid(),
		CWD: "/w/Polytrader/api", GitRoot: "/w/Polytrader/api"}, "100.0.0.2")

	svc.SetLocalPaneGroupsForHost("mac-one", map[string]string{uuidOne: "Window A"})
	svc.SetLocalPaneGroupsForHost("mac-two", map[string]string{uuidTwo: "Window B"})

	if g := groupOf(svc, one.PeerID); g != "Window A" {
		t.Errorf("host mac-one pane group = %q, want Window A", g)
	}
	if g := groupOf(svc, two.PeerID); g != "Window B" {
		t.Errorf("host mac-two pane group = %q, want Window B", g)
	}

	// mac-two's window closes and it pushes an empty map. Only its own entry goes.
	svc.SetLocalPaneGroupsForHost("mac-two", map[string]string{})
	if g := groupOf(svc, one.PeerID); g != "Window A" {
		t.Errorf("after another host's push, mac-one pane group = %q, want Window A", g)
	}
	if g := groupOf(svc, two.PeerID); g != "Polytrader" {
		t.Errorf("after its own empty push, mac-two pane group = %q, want the dirname fallback", g)
	}
}

// The same pane UUID on two hosts is two different panes. Keys carry the host so
// one host's map can never answer for another's pane.
func TestLocalPaneGroups_KeyedByHostNotPaneAlone(t *testing.T) {
	svc, _ := newTestService(t)
	svc.EnableFederation(nil, nil, func(ip string) (string, bool) {
		return "mac-one", ip == "100.0.0.1"
	})
	const uuid = "33333333-0000-0000-0000-000000000000"
	remote := svc.RegisterFrom(RegisterReq{LocalPaneID: uuid, PID: os.Getpid(),
		CWD: "/w/ChartLabs/backend", GitRoot: "/w/ChartLabs/backend"}, "100.0.0.1")

	// A push attributed to THIS daemon's own panes must not group a remote one.
	svc.SetLocalPaneGroups(map[string]string{uuid: "Local Window"})
	if g := groupOf(svc, remote.PeerID); g != "ChartLabs" {
		t.Errorf("remote pane group = %q, want the dirname fallback — a local push answered for another host", g)
	}
	svc.SetLocalPaneGroupsForHost("mac-one", map[string]string{uuid: "Remote Window"})
	if g := groupOf(svc, remote.PeerID); g != "Remote Window" {
		t.Errorf("remote pane group = %q, want Remote Window", g)
	}
}

func groupOf(svc *Service, peerID string) string {
	svc.mu.Lock()
	defer svc.mu.Unlock()
	return svc.groupOfLocked(svc.peers[peerID])
}

// A local pane's substrate IS its entry in the group map, so the map's key shape
// and the erasure test's lookup have to agree. When they drifted apart, every
// driver-mode pane read as destroyed and its mailbox — undelivered mail included
// — was erased after the grace window, while the pane was sitting right there.
func TestLocalPaneSubstrate_SurvivesWhenItsHostStillPushesTheMap(t *testing.T) {
	svc, _ := newTestService(t)
	svc.EnableFederation(nil, nil, func(ip string) (string, bool) {
		return "mac-one", ip == "100.0.0.1"
	})
	now := time.Unix(1_700_000_000, 0)
	svc.Now = func() time.Time { return now }

	const uuid = "44444444-0000-0000-0000-000000000000"
	remote := svc.RegisterFrom(RegisterReq{LocalPaneID: uuid, PID: os.Getpid(),
		CWD: "/w/ChartLabs/backend", GitRoot: "/w/ChartLabs/backend"}, "100.0.0.1")
	svc.SetLocalPaneGroupsForHost("mac-one", map[string]string{uuid: "Window A"})

	// Well past the grace window: the pane is still in its host's map, so nothing
	// about it is missing and the mailbox must survive.
	svc.Unregister(remote.PeerID)
	now = now.Add(substrateGrace + time.Minute)
	svc.ReapOnce()
	if _, err := svc.Poll(remote.PeerID); err != nil {
		t.Fatal("a local pane still present in its host's map was erased")
	}

	// Its host drops it from the map (pane closed, app quit) → now it is gone.
	svc.SetLocalPaneGroupsForHost("mac-one", map[string]string{})
	svc.ReapOnce()
	now = now.Add(substrateGrace + time.Minute)
	svc.ReapOnce()
	if _, err := svc.Poll(remote.PeerID); err == nil {
		t.Fatal("a local pane gone from its host's map should be reaped")
	}
}
