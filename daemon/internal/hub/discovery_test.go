package hub

import (
	"testing"

	"tailscale.com/ipn/ipnstate"
	"tailscale.com/types/key"
	"tailscale.com/types/views"
)

func TestMagicDNSLabel(t *testing.T) {
	cases := map[string]string{
		"hostb.tailb9053d.ts.net.": "hostb",
		"hostb.tailb9053d.ts.net":  "hostb",
		"solo.":                    "solo",
		"solo":                     "solo",
	}
	for in, want := range cases {
		if got := magicDNSLabel(in); got != want {
			t.Errorf("magicDNSLabel(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestHasTag(t *testing.T) {
	tags := []string{"tag:server", "tag:ccmux"}
	if !hasTag(tags, "tag:ccmux") {
		t.Error("want tag:ccmux found")
	}
	if hasTag(tags, "tag:missing") {
		t.Error("tag:missing should not be found")
	}
	if hasTag(nil, "tag:ccmux") {
		t.Error("nil tags contain nothing")
	}
}

func tagsPtr(ts ...string) *views.Slice[string] {
	v := views.SliceOf(ts)
	return &v
}

// TestNodesFromStatus: self is always a member; a tagged peer joins; an untagged
// peer and a DNS-less peer are excluded.
func TestNodesFromStatus(t *testing.T) {
	self := &ipnstate.PeerStatus{DNSName: "hub.tail0.ts.net."} // untagged self still counts
	tagged := &ipnstate.PeerStatus{DNSName: "hostb.tail0.ts.net.", Tags: tagsPtr("tag:ccmux")}

	st := &ipnstate.Status{
		Self: self,
		Peer: map[key.NodePublic]*ipnstate.PeerStatus{
			key.NewNode().Public(): tagged,
		},
	}
	got := nodesFromStatus(st, "tag:ccmux")
	if len(got) != 2 {
		t.Fatalf("nodes = %+v, want self + tagged peer", got)
	}
	byID := map[string]Node{}
	for _, n := range got {
		byID[n.ID] = n
	}
	if byID["hub"].Addr != "hub.tail0.ts.net" {
		t.Errorf("self addr = %q", byID["hub"].Addr)
	}
	if byID["hostb"].Addr != "hostb.tail0.ts.net" {
		t.Errorf("peer addr = %q", byID["hostb"].Addr)
	}

	// Untagged peer excluded.
	st2 := &ipnstate.Status{
		Peer: map[key.NodePublic]*ipnstate.PeerStatus{
			key.NewNode().Public(): {DNSName: "other.tail0.ts.net.", Tags: tagsPtr("tag:server")},
		},
	}
	if got := nodesFromStatus(st2, "tag:ccmux"); len(got) != 0 {
		t.Fatalf("untagged peer should be excluded, got %+v", got)
	}
}

// TestHubURLFromStatus: a tag:ccmux-hub peer is the hub; an untagged/self node is not.
func TestHubURLFromStatus(t *testing.T) {
	hubPeer := &ipnstate.PeerStatus{DNSName: "hub.tail0.ts.net.", Tags: tagsPtr("tag:ccmux", "tag:ccmux-hub")}
	plain := &ipnstate.PeerStatus{DNSName: "devbox.tail0.ts.net.", Tags: tagsPtr("tag:ccmux")}
	st := &ipnstate.Status{
		Self: plain, // this node is a plain member, not the hub
		Peer: map[key.NodePublic]*ipnstate.PeerStatus{key.NewNode().Public(): hubPeer},
	}
	if got := hubURLFromStatus(st, "devbox"); got != "https://hub.tail0.ts.net" {
		t.Fatalf("hub url = %q, want https://hub.tail0.ts.net", got)
	}

	// A node that is ITSELF the hub discovers no (other) hub.
	stSelfHub := &ipnstate.Status{Self: &ipnstate.PeerStatus{DNSName: "hub.tail0.ts.net.", Tags: tagsPtr("tag:ccmux-hub")}}
	if got := hubURLFromStatus(stSelfHub, "hub"); got != "" {
		t.Fatalf("self-hub should find no other hub, got %q", got)
	}

	// No hub-tagged node anywhere.
	stNone := &ipnstate.Status{Self: plain}
	if got := hubURLFromStatus(stNone, "devbox"); got != "" {
		t.Fatalf("no hub tag → %q, want empty", got)
	}
}

func TestIsMemberIP(t *testing.T) {
	r := NewRegistry("hub", 1,
		func() ([]Node, error) {
			return []Node{{ID: "hub", Addr: "hub.ts.net", IPs: []string{"100.0.0.1"}},
				{ID: "b", Addr: "b.ts.net", IPs: []string{"100.0.0.2", "fd7a::2"}}}, nil
		},
		func(string) (Health, error) { return Health{Contract: 1}, nil },
		func() int64 { return 1 },
	)
	r.Refresh()
	if !r.IsMemberIP("100.0.0.2") || !r.IsMemberIP("fd7a::2") {
		t.Error("member IPs should be recognized")
	}
	if r.IsMemberIP("100.9.9.9") {
		t.Error("non-member IP must be rejected")
	}
}

// TestHubOwner: the tag names the owner whether it sits on self or a peer, and
// an untagged fleet has no owner at all. This is the devhost DNS-ownership
// input, where "I am the hub" and "there is no hub" must stay distinguishable.
func TestHubOwner(t *testing.T) {
	self := &ipnstate.PeerStatus{DNSName: "hosta.tail0.ts.net.", Tags: tagsPtr(CcmuxTag)}
	hubPeer := &ipnstate.PeerStatus{DNSName: "hub.tail0.ts.net.", Tags: tagsPtr(CcmuxTag, HubTag)}
	peerKey := key.NewNode().Public()

	st := &ipnstate.Status{Self: self, Peer: map[key.NodePublic]*ipnstate.PeerStatus{peerKey: hubPeer}}
	if got := HubOwner(st); got != "hub" {
		t.Errorf("HubOwner with a hub peer = %q, want hub", got)
	}

	// Self holding the tag names itself — unlike DiscoverHub, which returns ""
	// for both this case and the no-hub one.
	st = &ipnstate.Status{
		Self: &ipnstate.PeerStatus{DNSName: "hub.tail0.ts.net.", Tags: tagsPtr(HubTag)},
		Peer: map[key.NodePublic]*ipnstate.PeerStatus{peerKey: self},
	}
	if got := HubOwner(st); got != "hub" {
		t.Errorf("HubOwner with a hub self = %q, want hub", got)
	}

	st = &ipnstate.Status{Self: self, Peer: map[key.NodePublic]*ipnstate.PeerStatus{peerKey: self}}
	if got := HubOwner(st); got != "" {
		t.Errorf("HubOwner with no hub = %q, want empty", got)
	}
	if got := HubOwner(nil); got != "" {
		t.Errorf("HubOwner(nil) = %q, want empty", got)
	}
}

// TestHubOwner_LowestTaggedLabelWins: with two tagged nodes every node in the
// fleet must name the same one, or both would claim the shared record and
// overwrite each other forever.
func TestHubOwner_LowestTaggedLabelWins(t *testing.T) {
	self := &ipnstate.PeerStatus{DNSName: "zulu.tail0.ts.net.", Tags: tagsPtr(CcmuxTag, HubTag)}
	peer := &ipnstate.PeerStatus{DNSName: "alpha.tail0.ts.net.", Tags: tagsPtr(CcmuxTag, HubTag)}
	st := &ipnstate.Status{
		Self: self,
		Peer: map[key.NodePublic]*ipnstate.PeerStatus{key.NewNode().Public(): peer},
	}
	if got := HubOwner(st); got != "alpha" {
		t.Errorf("HubOwner with two hubs = %q, want alpha from both sides", got)
	}
	// And the same status read from alpha's side agrees.
	flipped := &ipnstate.Status{
		Self: peer,
		Peer: map[key.NodePublic]*ipnstate.PeerStatus{key.NewNode().Public(): self},
	}
	if got := HubOwner(flipped); got != "alpha" {
		t.Errorf("HubOwner from the other side = %q, want alpha", got)
	}
}

// TestHubOwner_OfflineHubKeepsIt is a deliberate choice, not an oversight: only
// the hub can serve the whole fleet's dev hostnames, so a member seizing the
// record while the hub is down would swap one broken state for a narrower one
// and then flap when the hub comes back.
func TestHubOwner_OfflineHubKeepsIt(t *testing.T) {
	st := &ipnstate.Status{
		Self: &ipnstate.PeerStatus{DNSName: "hosta.tail0.ts.net.", Tags: tagsPtr(CcmuxTag)},
		Peer: map[key.NodePublic]*ipnstate.PeerStatus{
			key.NewNode().Public(): {
				DNSName: "hub.tail0.ts.net.", Tags: tagsPtr(CcmuxTag, HubTag),
				Online: false, Expired: true,
			},
		},
	}
	if got := HubOwner(st); got != "hub" {
		t.Errorf("HubOwner with an offline hub = %q, want hub", got)
	}
	if owns, _ := DevDNSOwner(st, "hosta", false); owns {
		t.Error("a member claimed the record while the hub was merely offline")
	}
}

// TestDevDNSOwner covers the fallbacks that apply when no node carries the hub
// tag — the configuration where every daemon used to claim the record.
func TestDevDNSOwner(t *testing.T) {
	self := &ipnstate.PeerStatus{DNSName: "hosta.tail0.ts.net.", Tags: tagsPtr(CcmuxTag)}
	member := &ipnstate.PeerStatus{DNSName: "hostb.tail0.ts.net.", Tags: tagsPtr(CcmuxTag)}
	hubPeer := &ipnstate.PeerStatus{DNSName: "hub.tail0.ts.net.", Tags: tagsPtr(CcmuxTag, HubTag)}
	withPeers := func(self *ipnstate.PeerStatus, peers ...*ipnstate.PeerStatus) *ipnstate.Status {
		st := &ipnstate.Status{Self: self, Peer: map[key.NodePublic]*ipnstate.PeerStatus{}}
		for _, p := range peers {
			st.Peer[key.NewNode().Public()] = p
		}
		return st
	}

	// The tag decides, whichever side asks.
	if owns, _ := DevDNSOwner(withPeers(hubPeer, member), "hub", true); !owns {
		t.Error("the tagged hub does not own the record")
	}
	if owns, reason := DevDNSOwner(withPeers(self, hubPeer), "hosta", false); owns || reason == "" {
		t.Errorf("member owns = %v, reason = %q, want a refusal naming the hub", owns, reason)
	}

	// Untagged fleet: the daemon running the hub role claims it, and says the
	// tag is missing. A member next to it defers rather than fight for it.
	owns, reason := DevDNSOwner(withPeers(self, member), "hosta", true)
	if !owns || reason == "" {
		t.Errorf("untagged hub owns = %v, reason = %q, want ownership plus a warning", owns, reason)
	}
	if owns, reason := DevDNSOwner(withPeers(self, member), "hosta", false); owns || reason == "" {
		t.Errorf("untagged member owns = %v, reason = %q, want a refusal", owns, reason)
	}

	// A lone daemon owns its record with nothing to report.
	if owns, reason := DevDNSOwner(withPeers(self), "hosta", false); !owns || reason != "" {
		t.Errorf("solo daemon owns = %v, reason = %q, want a quiet yes", owns, reason)
	}
}
