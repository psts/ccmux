package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

// hubStub answers the host-token mint and records the local-groups PUT.
func hubStub(t *testing.T, onPut func(r *http.Request, body []byte)) (*httptest.Server, *atomic.Pointer[string]) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/peers/host-token" {
			_ = json.NewEncoder(w).Encode(map[string]string{"token": "hub-secret"})
			return
		}
		body, _ := io.ReadAll(r.Body)
		onPut(r, body)
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
	}))
	t.Cleanup(srv.Close)
	p := &atomic.Pointer[string]{}
	u := srv.URL
	p.Store(&u)
	return srv, p
}

// The forward has to match peersLocalGroups exactly — verb, path, body key and
// bearer. Any mismatch fails the push, and the only symptom is one log line plus
// driver-mode sessions silently grouping by directory on the hub.
func TestLocalGroupsForwarder_SpeaksTheHandlersContract(t *testing.T) {
	type got struct {
		method, path, auth string
		groups             map[string]string
	}
	seen := make(chan got, 4)
	_, hubURL := hubStub(t, func(r *http.Request, body []byte) {
		var payload struct {
			Groups map[string]string `json:"groups"`
		}
		_ = json.Unmarshal(body, &payload)
		seen <- got{r.Method, r.URL.Path, r.Header.Get("Authorization"), payload.Groups}
	})

	client := &http.Client{Timeout: 3 * time.Second}
	cred := &hubHostCredential{client: client, hubURL: hubURL}
	f := newLocalGroupsForwarder(client, hubURLReader(hubURL), cred)
	f.Submit(map[string]string{"pane-uuid": "Window A"})

	select {
	case g := <-seen:
		if g.method != http.MethodPut {
			t.Errorf("method = %s, want PUT", g.method)
		}
		if g.path != "/v1/peers/local-groups" {
			t.Errorf("path = %s, want /v1/peers/local-groups", g.path)
		}
		if g.auth != "Bearer hub-secret" {
			t.Errorf("auth = %q, want the hub's own credential", g.auth)
		}
		if g.groups["pane-uuid"] != "Window A" {
			t.Errorf("groups = %v, want the submitted map under the \"groups\" key", g.groups)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("the map was never forwarded")
	}
}

// No hub discovered is not a failure to report — there is simply nowhere to send
// it, and every member spends its single-host life in this state.
func TestLocalGroupsForwarder_NoHubIsSilent(t *testing.T) {
	reached := make(chan struct{}, 1)
	_, hubURL := hubStub(t, func(*http.Request, []byte) { reached <- struct{}{} })
	empty := ""
	hubURL.Store(&empty)

	client := &http.Client{Timeout: time.Second}
	f := newLocalGroupsForwarder(client, hubURLReader(hubURL), &hubHostCredential{client: client, hubURL: hubURL})
	f.Submit(map[string]string{"pane-uuid": "Window A"})

	select {
	case <-reached:
		t.Fatal("a push was attempted with no hub discovered")
	case <-time.After(300 * time.Millisecond):
	}
}

// Submit is called from an HTTP handler and must never block it, even with the
// worker busy and the slot already full.
func TestLocalGroupsForwarder_SubmitNeverBlocks(t *testing.T) {
	release := make(chan struct{})
	_, hubURL := hubStub(t, func(*http.Request, []byte) { <-release })
	client := &http.Client{Timeout: 5 * time.Second}
	f := newLocalGroupsForwarder(client, hubURLReader(hubURL), &hubHostCredential{client: client, hubURL: hubURL})

	done := make(chan struct{})
	go func() {
		for i := 0; i < 50; i++ {
			f.Submit(map[string]string{"pane-uuid": "Window A"})
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("Submit blocked the caller")
	}
	close(release)
}
