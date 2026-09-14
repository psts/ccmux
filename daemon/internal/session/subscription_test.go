package session

import "testing"

// Pins the %subscription-changed parser. Wire format verified on tmux 3.6b:
//
//	%subscription-changed ccmux-title $0 @1 1 %5 : My Fancy Title
//
// (name, session, window, window-index, pane, " : ", value — the value may
// itself contain " : ", so only the first separator splits).
func TestHandleSubscriptionNotification(t *testing.T) {
	c := &Controller{
		byTmuxPane: map[string]*paneRef{"%5": {id: "pane-uuid", window: "@1", pane: "%5"}},
		byID:       map[string]*paneRef{},
		subs:       map[int]*subscriber{},
		notices:    make(chan Notice, 8),
	}

	c.OnNotification("subscription-changed", "ccmux-title $0 @1 1 %5 : ✳ Claude Code")
	c.OnNotification("subscription-changed", "ccmux-cmd $0 @1 1 %5 : zsh")
	c.OnNotification("subscription-changed", "ccmux-title $0 @1 1 %5 : a : b : c") // value keeps its separators
	c.OnNotification("subscription-changed", "ccmux-title $0 @9 9 %99 : orphan")   // unknown pane → dropped
	c.OnNotification("subscription-changed", "other-sub $0 @1 1 %5 : ignored")     // foreign subscription → dropped
	c.OnNotification("subscription-changed", "garbage-without-separator")          // malformed → dropped

	want := []Notice{
		{Kind: "pane-title", PaneID: "pane-uuid", Value: "✳ Claude Code"},
		{Kind: "pane-command", PaneID: "pane-uuid", Value: "zsh"},
		{Kind: "pane-title", PaneID: "pane-uuid", Value: "a : b : c"},
	}
	for i, w := range want {
		select {
		case got := <-c.notices:
			if got.Kind != w.Kind || got.PaneID != w.PaneID || got.Value != w.Value {
				t.Fatalf("notice %d = %+v, want %+v", i, got, w)
			}
		default:
			t.Fatalf("missing notice %d (%+v)", i, w)
		}
	}
	select {
	case extra := <-c.notices:
		t.Fatalf("unexpected extra notice %+v", extra)
	default:
	}
}

// Leaving the alternate screen is not a notice for the manager: it is a repaint
// request fanned out to the attached lenses (alternateScreenChanged). Entering
// asks nothing of anyone, a pane's first "0" is not a leave, and an
// unregistered pane is dropped like any other.
func TestHandleSubscription_LeavingAlternateScreenRepaints(t *testing.T) {
	c := &Controller{
		byTmuxPane: map[string]*paneRef{"%5": {id: "pane-uuid", window: "@1", pane: "%5"}},
		byID:       map[string]*paneRef{},
		subs:       map[int]*subscriber{},
		notices:    make(chan Notice, 8),
	}
	sub := c.Subscribe()
	defer sub.Close()

	c.OnNotification("subscription-changed", "ccmux-alt $0 @1 1 %5 : 0")  // first value, lazily pushed: not a leave
	c.OnNotification("subscription-changed", "ccmux-alt $0 @1 1 %5 : 1")  // entering: the program paints itself
	c.OnNotification("subscription-changed", "ccmux-alt $0 @9 9 %99 : 0") // unknown pane → dropped
	c.OnNotification("subscription-changed", "ccmux-alt $0 @1 1 %5 : 0")  // leaving: repaint

	select {
	case ev := <-sub.C:
		if ev.Kind != "repaint" || ev.PaneID != "pane-uuid" {
			t.Fatalf("event = %+v, want repaint for pane-uuid", ev)
		}
	default:
		t.Fatal("leaving the alternate screen fanned out no repaint")
	}
	select {
	case ev := <-sub.C:
		t.Fatalf("unexpected extra event %+v", ev)
	default:
	}
	select {
	case n := <-c.notices:
		t.Fatalf("alternate-screen change leaked a notice %+v", n)
	default:
	}
}
