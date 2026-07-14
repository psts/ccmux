package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"ccmux.dev/ccmuxd/internal/manager"
	"ccmux.dev/ccmuxd/internal/push"
	"ccmux.dev/ccmuxd/internal/store"
)

// pushTestServer builds an httptest server with push enabled over a real SQLite
// store. No tmux is needed — the /v1/push/* endpoints never touch a session.
func pushTestServer(t *testing.T) (*httptest.Server, store.Store) {
	t.Helper()
	st, err := store.Open(t.TempDir() + "/reg.db")
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	t.Cleanup(func() { st.Close() })

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	mgr := manager.New(ctx, nil, st)

	keys, err := push.LoadOrCreateKeys(t.TempDir() + "/vapid.json")
	if err != nil {
		t.Fatalf("vapid: %v", err)
	}
	srv := NewServer(mgr)
	srv.EnablePush(ctx, push.NewSender(keys, "mailto:test@ccmux.local"), st)

	hs := httptest.NewServer(srv.Handler())
	t.Cleanup(hs.Close)
	return hs, st
}

func TestAPI_PushVAPIDKey(t *testing.T) {
	hs, _ := pushTestServer(t)
	resp, err := http.Get(hs.URL + "/v1/push/vapid")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var got struct{ Key string }
	_ = json.NewDecoder(resp.Body).Decode(&got)
	if got.Key == "" {
		t.Error("vapid endpoint returned no public key")
	}
}

func TestAPI_PushSubscriptionCRUD(t *testing.T) {
	hs, st := pushTestServer(t)

	// Subscribe (self-declared user "alice" over loopback → keyed on "alice").
	subBody := `{"endpoint":"https://push.example/ep1","keys":{"p256dh":"BPk","auth":"c2Vj"}}`
	resp, err := http.Post(hs.URL+"/v1/push/subscriptions?user=alice", "application/json", strings.NewReader(subBody))
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("subscribe status = %d, want 201", resp.StatusCode)
	}
	resp.Body.Close()

	// The subscription is persisted (transport-generic address holds the endpoint).
	subs, _ := st.ListPushSubscriptions()
	if len(subs) != 1 || subs[0].Login != "alice" || !strings.Contains(subs[0].Address, "ep1") {
		t.Fatalf("stored subs = %+v, want one for alice with the endpoint", subs)
	}

	// Re-subscribing the same endpoint replaces rather than duplicates.
	resp, _ = http.Post(hs.URL+"/v1/push/subscriptions?user=alice", "application/json", strings.NewReader(subBody))
	resp.Body.Close()
	if subs, _ := st.ListPushSubscriptions(); len(subs) != 1 {
		t.Fatalf("after re-subscribe = %d subs, want 1 (dedup by endpoint)", len(subs))
	}

	// List returns alice's own subscriptions without leaking encryption keys.
	resp, err = http.Get(hs.URL + "/v1/push/subscriptions?user=alice")
	if err != nil {
		t.Fatal(err)
	}
	var listed []map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&listed)
	resp.Body.Close()
	if len(listed) != 1 {
		t.Fatalf("list returned %d, want 1", len(listed))
	}
	if _, leaked := listed[0]["address"]; leaked {
		t.Error("list leaked the subscription address/keys")
	}

	// A different dev sees none of alice's subscriptions.
	resp, _ = http.Get(hs.URL + "/v1/push/subscriptions?user=bob")
	var bobList []map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&bobList)
	resp.Body.Close()
	if len(bobList) != 0 {
		t.Errorf("bob saw %d of alice's subs, want 0", len(bobList))
	}

	// Unsubscribe by endpoint.
	req, _ := http.NewRequest(http.MethodDelete, hs.URL+"/v1/push/subscriptions", strings.NewReader(`{"endpoint":"https://push.example/ep1"}`))
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("delete status = %d, want 204", resp.StatusCode)
	}
	if subs, _ := st.ListPushSubscriptions(); len(subs) != 0 {
		t.Fatalf("after delete = %d subs, want 0", len(subs))
	}
}

func TestAPI_PushSubscriptionRejectsIncomplete(t *testing.T) {
	hs, _ := pushTestServer(t)
	resp, err := http.Post(hs.URL+"/v1/push/subscriptions", "application/json", strings.NewReader(`{"endpoint":"x"}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want 400 (missing keys)", resp.StatusCode)
	}
}

// TestAPI_PushDisabledWhenNotEnabled confirms the endpoints degrade to 503 when a
// server has no push wired (e.g. VAPID init failed), rather than panicking.
func TestAPI_PushDisabledWhenNotEnabled(t *testing.T) {
	mgr := manager.New(context.Background(), nil, nil)
	hs := httptest.NewServer(NewServer(mgr).Handler())
	defer hs.Close()

	resp, err := http.Get(hs.URL + "/v1/push/vapid")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503 when push disabled", resp.StatusCode)
	}
}
