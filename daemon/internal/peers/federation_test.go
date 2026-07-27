package peers

import "testing"

// TestFederation_GlobalGroupAndHostStamp: a pane the LOCAL manager doesn't know
// (it lives on another host) resolves its window group via the hub's global
// resolver, and its owning host is stamped onto the peer for list_peers.
func TestFederation_GlobalGroupAndHostStamp(t *testing.T) {
	svc, _ := newTestService(t)
	svc.EnableFederation(
		func(paneID string) (string, bool) {
			if paneID == "remote-pane" {
				return "CHARTLABS", true
			}
			return "", false
		},
		func(paneID string) (string, bool) {
			if paneID == "remote-pane" {
				return "devbox", true
			}
			return "", false
		},
	)

	resp := registerPane(svc, "remote-pane", "/srv/x/backend")
	if resp.Group != "CHARTLABS" {
		t.Fatalf("remote pane group = %q, want CHARTLABS (via global resolver)", resp.Group)
	}
	svc.mu.Lock()
	p := svc.peers[resp.PeerID]
	svc.mu.Unlock()
	if p == nil || p.Host != "devbox" {
		t.Fatalf("peer.Host = %+v, want devbox", p)
	}
}

// TestFederation_LocalGroupWins: the local manager takes precedence over the
// global resolver, so a pane on the hub itself resolves locally.
func TestFederation_LocalGroupWins(t *testing.T) {
	svc, hook := newTestService(t)
	hook.groups["local-pane"] = "LOCALWIN"
	svc.EnableFederation(
		func(string) (string, bool) { return "GLOBAL", true },
		func(string) (string, bool) { return "otherhost", true },
	)
	if resp := registerPane(svc, "local-pane", "/x"); resp.Group != "LOCALWIN" {
		t.Fatalf("local pane group = %q, want LOCALWIN (local manager wins)", resp.Group)
	}
}

// TestFederation_DisabledIsInert: with no resolvers wired (single-host), the
// federation additions are inert — a peer's Host stays "" and grouping is
// whatever the pre-federation path produced (still non-empty here).
func TestFederation_DisabledIsInert(t *testing.T) {
	svc, _ := newTestService(t)
	resp := registerPane(svc, "lonely-pane", "/Users/x/Work/backend")
	if resp.Group == "" {
		t.Fatal("expected the unchanged fallback group, got empty")
	}
	svc.mu.Lock()
	p := svc.peers[resp.PeerID]
	svc.mu.Unlock()
	if p.Host != "" {
		t.Fatalf("peer.Host = %q, want empty off the hub", p.Host)
	}
}
