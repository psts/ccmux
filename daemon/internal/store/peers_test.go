package store

import (
	"fmt"
	"testing"

	"ccmux.dev/ccmuxd/internal/model"
)

func msgEvent(from, to, group, text string, sentAt int64) *model.PeerEvent {
	return &model.PeerEvent{
		Kind: model.PeerEventMessage, FromID: from, ToID: to,
		Group: group, Text: text, SentAt: sentAt,
	}
}

func TestPeerEvents_AppendAndReplayAfterCursor(t *testing.T) {
	st := openTestStore(t)
	for i := 1; i <= 3; i++ {
		seq, err := st.AppendPeerEvent(msgEvent("a", "b", "G", fmt.Sprintf("m%d", i), int64(i)))
		if err != nil {
			t.Fatalf("append: %v", err)
		}
		if seq != int64(i) {
			t.Fatalf("seq = %d, want %d", seq, i)
		}
	}
	// Event for another peer must not appear in b's replay.
	if _, err := st.AppendPeerEvent(msgEvent("a", "c", "G", "other", 4)); err != nil {
		t.Fatal(err)
	}

	evs, err := st.PeerEventsAfter("b", 1)
	if err != nil {
		t.Fatalf("events after: %v", err)
	}
	if len(evs) != 2 || evs[0].Text != "m2" || evs[1].Text != "m3" {
		t.Fatalf("replay = %+v, want m2,m3 in order", evs)
	}
}

func TestPeerCursor_AdvanceIsMonotonic(t *testing.T) {
	st := openTestStore(t)
	if c, err := st.PeerCursor("p"); err != nil || c != 0 {
		t.Fatalf("fresh cursor = %d, %v; want 0, nil", c, err)
	}
	if err := st.AdvancePeerCursor("p", 5); err != nil {
		t.Fatal(err)
	}
	// A stale (lower) ack must not rewind.
	if err := st.AdvancePeerCursor("p", 3); err != nil {
		t.Fatal(err)
	}
	if c, _ := st.PeerCursor("p"); c != 5 {
		t.Fatalf("cursor = %d, want 5 (no rewind)", c)
	}
	if err := st.AdvancePeerCursor("p", 9); err != nil {
		t.Fatal(err)
	}
	if c, _ := st.PeerCursor("p"); c != 9 {
		t.Fatalf("cursor = %d, want 9", c)
	}
}

func TestRecentPeerSenders_WindowKindAndDistinct(t *testing.T) {
	st := openTestStore(t)
	appendAll := func(evs ...*model.PeerEvent) {
		t.Helper()
		for _, ev := range evs {
			if _, err := st.AppendPeerEvent(ev); err != nil {
				t.Fatal(err)
			}
		}
	}
	appendAll(
		msgEvent("old", "w", "G", "too old", 100),
		msgEvent("a", "w", "G", "hi", 1000),
		msgEvent("a", "w", "G", "again", 1500), // duplicate sender → one row
		msgEvent("b", "w", "G", "yo", 2000),
		msgEvent("c", "other", "G", "not for w", 2000),
		// A verdict inside the window is not a "message" → excluded.
		&model.PeerEvent{Kind: model.PeerEventVerdict, FromID: "v", ToID: "w",
			Group: "G", Text: "yes abcde", RequestID: "abcde", Behavior: "allow", SentAt: 2000},
	)
	got, err := st.RecentPeerSenders("w", 500)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]bool{"a": true, "b": true}
	if len(got) != 2 || !want[got[0]] || !want[got[1]] {
		t.Fatalf("recent senders = %v, want exactly {a,b}", got)
	}
}

func TestPeerGroupMessages_SnapshotGroupLimitAndOrder(t *testing.T) {
	st := openTestStore(t)
	for i := 1; i <= 5; i++ {
		if _, err := st.AppendPeerEvent(msgEvent("a", "b", "G", fmt.Sprintf("m%d", i), int64(i*100))); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := st.AppendPeerEvent(msgEvent("x", "y", "OTHER", "elsewhere", 300)); err != nil {
		t.Fatal(err)
	}
	// Verdict events are messages to the viewer? No — kind filter excludes them.
	if _, err := st.AppendPeerEvent(&model.PeerEvent{
		Kind: model.PeerEventVerdict, FromID: "a", ToID: "b", Group: "G",
		Text: "yes abcde", RequestID: "abcde", Behavior: "allow", SentAt: 600,
	}); err != nil {
		t.Fatal(err)
	}

	got, err := st.PeerGroupMessages("G", 150, 3)
	if err != nil {
		t.Fatal(err)
	}
	// since=150 excludes m1; limit=3 keeps the newest three of m2..m5,
	// returned oldest-first.
	if len(got) != 3 || got[0].Text != "m3" || got[1].Text != "m4" || got[2].Text != "m5" {
		t.Fatalf("group messages = %+v, want m3,m4,m5", got)
	}
}

func TestPrunePeerEvents_RespectsCursorPerAddressee(t *testing.T) {
	st := openTestStore(t)
	// Old + acked → prunable. Old + unacked → kept. New + acked → kept.
	if _, err := st.AppendPeerEvent(msgEvent("a", "acked", "G", "old-acked", 100)); err != nil { // seq 1
		t.Fatal(err)
	}
	if _, err := st.AppendPeerEvent(msgEvent("a", "unacked", "G", "old-unacked", 100)); err != nil { // seq 2
		t.Fatal(err)
	}
	if _, err := st.AppendPeerEvent(msgEvent("a", "acked", "G", "new-acked", 900)); err != nil { // seq 3
		t.Fatal(err)
	}
	if err := st.AdvancePeerCursor("acked", 3); err != nil {
		t.Fatal(err)
	}

	n, err := st.PrunePeerEvents(500)
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("pruned %d, want 1 (only old+acked)", n)
	}
	if evs, _ := st.PeerEventsAfter("unacked", 0); len(evs) != 1 || evs[0].Text != "old-unacked" {
		t.Fatalf("unacked event lost: %+v", evs)
	}
	if evs, _ := st.PeerEventsAfter("acked", 0); len(evs) != 1 || evs[0].Text != "new-acked" {
		t.Fatalf("acked peer's remaining events = %+v, want just new-acked", evs)
	}
}
