package store

import (
	"path/filepath"
	"testing"

	"ccmux.dev/ccmuxd/internal/model"
)

func openTestStore(t *testing.T) *SQLite {
	t.Helper()
	st, err := Open(filepath.Join(t.TempDir(), "reg.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

func TestPushSubscriptions_CRUD(t *testing.T) {
	st := openTestStore(t)

	a := &model.PushSubscription{ID: "id-a", Login: "alice@example.com", Transport: "webpush", Address: `{"endpoint":"a"}`, CreatedAt: 1}
	b := &model.PushSubscription{ID: "id-b", Login: "bob@example.com", Transport: "webpush", Address: `{"endpoint":"b"}`, CreatedAt: 2}
	for _, s := range []*model.PushSubscription{a, b} {
		if err := st.SavePushSubscription(s); err != nil {
			t.Fatalf("save %s: %v", s.ID, err)
		}
	}

	got, err := st.ListPushSubscriptions()
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("list returned %d, want 2", len(got))
	}

	// Re-saving the same id replaces (upsert), not duplicates.
	a.Address = `{"endpoint":"a2"}`
	if err := st.SavePushSubscription(a); err != nil {
		t.Fatalf("re-save: %v", err)
	}
	got, _ = st.ListPushSubscriptions()
	if len(got) != 2 {
		t.Fatalf("after re-save list = %d, want 2 (upsert)", len(got))
	}
	if addr := findAddress(got, "id-a"); addr != `{"endpoint":"a2"}` {
		t.Errorf("id-a address = %q, want updated", addr)
	}

	// Delete is by id and idempotent.
	if err := st.DeletePushSubscription("id-a"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if err := st.DeletePushSubscription("id-a"); err != nil {
		t.Fatalf("second delete should be a no-op error-free: %v", err)
	}
	got, _ = st.ListPushSubscriptions()
	if len(got) != 1 || got[0].ID != "id-b" {
		t.Fatalf("after delete list = %+v, want only id-b", got)
	}
}

// TestPushSubscriptions_SurviveReopen confirms subscriptions persist across a
// daemon restart (a device stays subscribed even if the daemon bounces).
func TestPushSubscriptions_SurviveReopen(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "reg.db")

	st1, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := st1.SavePushSubscription(&model.PushSubscription{ID: "x", Login: "u", Transport: "webpush", Address: `{"endpoint":"e"}`}); err != nil {
		t.Fatal(err)
	}
	st1.Close()

	st2, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer st2.Close()
	got, _ := st2.ListPushSubscriptions()
	if len(got) != 1 || got[0].ID != "x" {
		t.Fatalf("after reopen = %+v, want the persisted subscription", got)
	}
}

func findAddress(subs []*model.PushSubscription, id string) string {
	for _, s := range subs {
		if s.ID == id {
			return s.Address
		}
	}
	return ""
}
