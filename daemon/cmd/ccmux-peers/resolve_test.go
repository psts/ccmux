package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
)

// busStub stands in for the pane's own daemon: it answers /v1/peers/bus with
// whatever the test currently wants, and records every unregister it receives.
type busStub struct {
	mu           sync.Mutex
	url, token   string
	status       int
	unregistered int
	asks         int
}

func (b *busStub) answer(url, token string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.url, b.token = url, token
}

func (b *busStub) counts() (asks, unregistered int) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.asks, b.unregistered
}

func (b *busStub) server(t *testing.T) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/peers/bus", func(w http.ResponseWriter, r *http.Request) {
		b.mu.Lock()
		if b.status != 0 && b.status != http.StatusOK {
			code := b.status
			b.asks++
			b.mu.Unlock()
			w.WriteHeader(code)
			_, _ = w.Write([]byte(`{"error":"nope"}`))
			return
		}
		url, token := b.url, b.token
		b.asks++
		b.mu.Unlock()
		_ = json.NewEncoder(w).Encode(map[string]string{"url": url, "token": token})
	})
	mux.HandleFunc("/v1/peers/unregister", func(w http.ResponseWriter, r *http.Request) {
		b.mu.Lock()
		b.unregistered++
		b.mu.Unlock()
		_, _ = w.Write([]byte(`{}`))
	})
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)
	return ts
}

func newResolveApp(t *testing.T, localURL string) *app {
	t.Helper()
	a := &app{mcp: newMCPServer(), paneID: "pane-1", localURL: localURL, localToken: "local-tok"}
	a.daemon = newDaemonClient(localURL, "local-tok")
	return a
}

// TestResolveBus_MovesToHub is the whole point of the change: a pane that
// started on its local bus joins the hub as soon as the daemon's tag discovery
// finds one, without the pane being recreated.
func TestResolveBus_MovesToHub(t *testing.T) {
	stub := &busStub{}
	ts := stub.server(t)
	a := newResolveApp(t, ts.URL)
	a.mu.Lock()
	a.id = "peer-1" // pretend we are registered locally
	a.mu.Unlock()

	stub.answer("https://hub.ts.net", "hub-tok")
	if !a.resolveBus() {
		t.Error("resolveBus reported no move — the watchdog would never cut the channel")
	}

	if url, token := a.daemon.target(); url != "https://hub.ts.net" || token != "hub-tok" {
		t.Errorf("target = %s/%s, want the hub", url, token)
	}
	// The old registration has to be dropped, or the bus we left keeps listing
	// us until its reaper times us out.
	if _, unreg := stub.counts(); unreg != 1 {
		t.Errorf("unregistered %d times on the OLD bus, want 1", unreg)
	}
	if a.peerID() != "" {
		t.Errorf("peer id = %q, want cleared so busLoop re-registers", a.peerID())
	}
}

// TestResolveBus_EmptyMeansStay: no hub discovered is not "no bus" — it means
// this daemon is the bus, and a repeat answer must not churn the registration.
func TestResolveBus_EmptyMeansStay(t *testing.T) {
	stub := &busStub{}
	ts := stub.server(t)
	a := newResolveApp(t, ts.URL)

	if a.resolveBus() || a.resolveBus() {
		t.Error("an empty answer reported a move — it means stay put")
	}

	if url, token := a.daemon.target(); url != ts.URL || token != "local-tok" {
		t.Errorf("target = %s/%s, want the local daemon", url, token)
	}
	if _, unreg := stub.counts(); unreg != 0 {
		t.Errorf("unregistered %d times while staying put, want 0", unreg)
	}
}

// TestResolveBus_StableAnswerDoesNotChurn: resolve runs before every
// registration, so an unchanged answer must be inert — otherwise every reconnect
// would drop and rebuild a perfectly good registration.
func TestResolveBus_StableAnswerDoesNotChurn(t *testing.T) {
	stub := &busStub{}
	ts := stub.server(t)
	a := newResolveApp(t, ts.URL)
	a.mu.Lock()
	a.id = "peer-1" // registered, so a move would have something to unregister
	a.mu.Unlock()
	stub.answer("https://hub.ts.net", "hub-tok")

	moved := 0
	for i := 0; i < 3; i++ {
		if a.resolveBus() {
			moved++
		}
	}
	if moved != 1 {
		t.Errorf("reported %d moves across 3 resolves, want exactly 1", moved)
	}

	asks, unreg := stub.counts()
	if asks != 3 {
		t.Errorf("asked %d times, want one per call", asks)
	}
	if unreg != 1 {
		t.Errorf("unregistered %d times, want exactly 1 (the initial move)", unreg)
	}
}

// TestResolveBus_FailureStaysPut covers the rollout case: an older daemon 404s
// this route. Staying on the current bus is the only safe answer.
func TestResolveBus_FailureStaysPut(t *testing.T) {
	stub := &busStub{status: http.StatusNotFound}
	ts := stub.server(t)
	a := newResolveApp(t, ts.URL)

	if a.resolveBus() {
		t.Error("a failed resolve reported a move")
	}

	if url, _ := a.daemon.target(); url != ts.URL {
		t.Errorf("target = %s, want to stay on the local daemon", url)
	}
	if _, unreg := stub.counts(); unreg != 0 {
		t.Errorf("unregistered %d times on a failed resolve, want 0", unreg)
	}
}

// TestResolveBus_SkipsWhenNotResolvable: a pane-less session has no pane token
// to authorize with, and a legacy-stamped pane holds a token only the hub
// accepts. Both must not ask.
func TestResolveBus_SkipsWhenNotResolvable(t *testing.T) {
	for _, tc := range []struct{ name, localURL, pane string }{
		{"pane-less or legacy-stamped: localURL cleared", "", "pane-1"},
		{"no pane id to authorize with", "http://127.0.0.1:1", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stub := &busStub{}
			ts := stub.server(t)
			a := newResolveApp(t, ts.URL)
			a.localURL, a.paneID = tc.localURL, tc.pane

			a.resolveBus()

			if asks, _ := stub.counts(); asks != 0 {
				t.Errorf("asked %d times, want 0 — nothing here can be authorized", asks)
			}
		})
	}
}
