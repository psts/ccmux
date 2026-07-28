package main

import "testing"

// Every listed peer has a live session behind it, so status only says how fast
// it will hear you. A poll-only session holds no socket BY DESIGN — reporting
// that as "reconnecting" forever was a permanent lie about a healthy peer.
func TestPeerStatus(t *testing.T) {
	cases := []struct {
		name string
		in   listEntry
		want string
	}{
		{"push, socket up", listEntry{Connected: true}, "online"},
		{"push, socket dropped", listEntry{}, "online (reconnecting)"},
		{"poll-only", listEntry{PollOnly: true}, "online (polls for messages — delivery on its next check)"},
		{
			"poll-only never reads as reconnecting even without a socket",
			listEntry{PollOnly: true, Connected: false},
			"online (polls for messages — delivery on its next check)",
		},
	}
	for _, c := range cases {
		if got := peerStatus(c.in); got != c.want {
			t.Errorf("%s: peerStatus = %q, want %q", c.name, got, c.want)
		}
	}
}
