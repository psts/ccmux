package api

import (
	"context"
	"testing"

	"ccmux.dev/ccmuxd/internal/manager"
)

// newTestHub builds a presenceHub over an empty Manager. Join/Input/Driver never
// touch the tmux server or store (broadcast no-ops on an unknown workspace), so
// nil dependencies are safe for these pure presence tests.
func newTestHub() *presenceHub {
	return newPresenceHub(manager.New(context.Background(), nil, nil))
}

func TestPresence_DriverTracksLastNonReadonlyTypist(t *testing.T) {
	h := newTestHub()
	alice := h.Join("ws1", ClientInfo{User: "Alice", Verified: true}, "alice@example.com")
	bob := h.Join("ws1", ClientInfo{User: "Bob"}, "")

	if _, ok := h.Driver("ws1"); ok {
		t.Fatal("no driver expected before any input")
	}

	h.Input("ws1", alice)
	d, ok := h.Driver("ws1")
	if !ok || d.User != "Alice" || d.Email != "alice@example.com" || !d.Verified {
		t.Fatalf("driver = %+v (ok=%v); want verified Alice with email", d, ok)
	}

	// Handoff: Bob types → Bob drives, and he has no verified email.
	h.Input("ws1", bob)
	d, _ = h.Driver("ws1")
	if d.User != "Bob" || d.Email != "" || d.Verified {
		t.Fatalf("driver after Bob types = %+v; want unverified Bob, no email", d)
	}
}

func TestPresence_DriverUnknownWorkspace(t *testing.T) {
	h := newTestHub()
	if _, ok := h.Driver("nope"); ok {
		t.Fatal("unknown workspace should have no driver")
	}
}

func TestPresence_DriverClearsWhenDriverLeaves(t *testing.T) {
	h := newTestHub()
	alice := h.Join("ws1", ClientInfo{User: "Alice"}, "")
	h.Input("ws1", alice)
	h.Leave("ws1", alice)
	if _, ok := h.Driver("ws1"); ok {
		t.Fatal("driver should clear when the driving client leaves")
	}
}
