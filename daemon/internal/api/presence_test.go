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
	alice := h.Join("ws1", ClientInfo{User: "Alice", Verified: true}, "alice@example.com", "alice@example.com")
	bob := h.Join("ws1", ClientInfo{User: "Bob"}, "Bob", "")

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

func TestPresence_ActiveOwnersDrivePushSuppression(t *testing.T) {
	h := newTestHub()
	alice := h.Join("ws1", ClientInfo{User: "Alice", Verified: true}, "alice@example.com", "alice@example.com")
	h.Join("ws2", ClientInfo{User: "Bob"}, "Bob", "") // attached elsewhere, never focuses

	// No one has focused a pane anywhere → nobody is suppressed.
	if owners := h.ActiveOwners(); len(owners) != 0 {
		t.Fatalf("owners before any focus = %v, want empty", owners)
	}

	// Alice focuses a pane in ws1 → she is demonstrably at a screen, so ALL her
	// pushes are suppressed — including attention from other workspaces (the
	// lens in front of her flashes those). Keyed by login, not display name
	// (which would falsely collide with a self-declared "Alice").
	h.Focus("ws1", alice, "pane-1")
	owners := h.ActiveOwners()
	if !owners["alice@example.com"] {
		t.Errorf("owners = %v, want Alice keyed by her login", owners)
	}
	if owners["Alice"] {
		t.Error("display name must not be a suppression key")
	}
	if owners["Bob"] {
		t.Error("Bob has no focused pane anywhere but was marked active")
	}

	// Alice clears focus (screen locked / tab hidden) → suppression lifts.
	h.Focus("ws1", alice, "")
	if owners := h.ActiveOwners(); len(owners) != 0 {
		t.Errorf("owners after focus cleared = %v, want empty", owners)
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
	alice := h.Join("ws1", ClientInfo{User: "Alice"}, "Alice", "")
	h.Input("ws1", alice)
	h.Leave("ws1", alice)
	if _, ok := h.Driver("ws1"); ok {
		t.Fatal("driver should clear when the driving client leaves")
	}
}
