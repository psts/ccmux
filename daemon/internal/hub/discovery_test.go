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
