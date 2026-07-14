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

func TestPresence_FocusedOwnersDrivePushSuppression(t *testing.T) {
	h := newTestHub()
	alice := h.Join("ws1", ClientInfo{User: "Alice", Verified: true}, "alice@example.com", "alice@example.com")
	h.Join("ws1", ClientInfo{User: "Bob"}, "Bob", "") // attached but never focuses

	// No one has focused a pane yet → nobody is suppressed.
	if owners := h.FocusedOwners("ws1"); len(owners) != 0 {
		t.Fatalf("owners before any focus = %v, want empty", owners)
	}

	// Alice focuses a pane → her login (email) is suppressed. The display name is
	// deliberately NOT a key (it would falsely collide with a self-declared "Alice").
	h.Focus("ws1", alice, "pane-1")
	owners := h.FocusedOwners("ws1")
	if !owners["alice@example.com"] {
		t.Errorf("owners = %v, want Alice keyed by her login", owners)
	}
	if owners["Alice"] {
		t.Error("display name must not be a suppression key")
	}
	if owners["Bob"] {
		t.Error("Bob has no focused pane but was marked focused")
	}

	// Alice clears focus (tab hidden) → suppression lifts.
	h.Focus("ws1", alice, "")
	if owners := h.FocusedOwners("ws1"); len(owners) != 0 {
		t.Errorf("owners after focus cleared = %v, want empty", owners)
	}
}

func TestPresence_FocusedOwnersUnknownWorkspace(t *testing.T) {
	h := newTestHub()
	if owners := h.FocusedOwners("nope"); owners != nil {
		t.Fatalf("unknown workspace owners = %v, want nil", owners)
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
