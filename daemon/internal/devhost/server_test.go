package devhost

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strconv"
	"strings"
	"sync"
	"testing"
)

// fakeState is an in-memory State. Cloudflare token stays empty in every test:
// a non-empty token would arm real ACME issuance in ensureCertLocked.
type fakeState struct {
	mu        sync.Mutex
	domain    string
	authKey   string
	hostnames map[string]int
	stamped   map[string]string // name → url from the last stamp
	listening map[int]bool      // port → listening from the last stamp
}

func (f *fakeState) DevDomain() string        { return f.domain }
func (f *fakeState) CloudflareToken() string  { return "" }
func (f *fakeState) TailscaleAuthKey() string { return f.authKey }
func (f *fakeState) AllHostnames() map[string]int {
	out := map[string]int{}
	for k, v := range f.hostnames {
		out[k] = v
	}
	return out
}
func (f *fakeState) StampHostnameRuntime(urlFor func(string) string, listeningFor func(int) bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.stamped, f.listening = map[string]string{}, map[int]bool{}
	for name, port := range f.hostnames {
		f.stamped[name] = urlFor(name)
		f.listening[port] = listeningFor(port)
	}
}

// fakeNode records reconcile decisions without touching the network.
type fakeNode struct{ closed bool }

func (n *fakeNode) Close() error { n.closed = true; return nil }

func testServer(t *testing.T, st *fakeState) (*Server, map[string]*fakeNode) {
	t.Helper()
	s := NewServer(context.Background(), st, t.TempDir(), "tailtest.ts.net", netip.Addr{})
	started := map[string]*fakeNode{}
	s.newNode = func(name, _ string) nodeHandle {
		n := &fakeNode{}
		started[name] = n
		return n
	}
	return s, started
}

func TestHandler_DomainModeDispatch(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, "backend saw host %s", r.Host)
	}))
	defer backend.Close()
	_, portStr, _ := net.SplitHostPort(strings.TrimPrefix(backend.URL, "http://"))
	port, _ := strconv.Atoi(portStr)

	st := &fakeState{domain: "dev.test", hostnames: map[string]int{"app": port}}
	s, _ := testServer(t, st)
	s.Refresh()

	next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { io.WriteString(w, "lens") })
	h := s.Handler(next)

	get := func(host string) (int, string) {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest("GET", "https://"+host+"/", nil)
		req.Host = host
		h.ServeHTTP(rec, req)
		return rec.Code, rec.Body.String()
	}

	// Routed host → proxied, preserving the public Host for vhosting dev servers.
	if code, body := get("app.dev.test"); code != 200 || body != "backend saw host app.dev.test" {
		t.Fatalf("routed = %d %q", code, body)
	}
	// Unknown name under the dev domain → friendly 404 listing known hosts.
	if code, body := get("typo.dev.test"); code != 404 || !strings.Contains(body, "https://app.dev.test") {
		t.Fatalf("unknown dev host = %d %q", code, body)
	}
	// Anything else falls through to the lens/API.
	if code, body := get("ccmuxd.tailtest.ts.net"); code != 200 || body != "lens" {
		t.Fatalf("fallthrough = %d %q", code, body)
	}
}

func TestHandler_BackendDown(t *testing.T) {
	// A mapped port with nothing listening answers 502, not a hung request.
	st := &fakeState{domain: "dev.test", hostnames: map[string]int{"app": freePort(t)}}
	s, _ := testServer(t, st)
	s.Refresh()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "https://app.dev.test/", nil)
	req.Host = "app.dev.test"
	s.Handler(http.NotFoundHandler()).ServeHTTP(rec, req)
	if rec.Code != 502 || !strings.Contains(rec.Body.String(), "isn't answering") {
		t.Fatalf("backend down = %d %q", rec.Code, rec.Body.String())
	}
}

func TestRefresh_ModesAndStamping(t *testing.T) {
	port := freePort(t)
	st := &fakeState{domain: "dev.test", hostnames: map[string]int{"app": port}}
	s, started := testServer(t, st)

	// Domain mode: table keys under the domain, URL stamped accordingly, no
	// nodes, cert status reflects the missing token (tests never arm ACME).
	s.Refresh()
	if _, ok := s.table.Load().Route("app.dev.test"); !ok {
		t.Fatal("domain-mode table missing app.dev.test")
	}
	if got := st.stamped["app"]; got != "https://app.dev.test" {
		t.Fatalf("stamped url = %q", got)
	}
	if st.listening[port] {
		t.Fatal("nothing listens on the free port, but stamped listening=true")
	}
	if len(started) != 0 {
		t.Fatalf("domain mode started nodes: %v", started)
	}
	if got := s.CertStatus(); !strings.HasPrefix(got, "error") {
		t.Fatalf("cert status without token = %q", got)
	}

	// Fallback mode: node per hostname, ts.net table + URLs, status unset.
	st.domain = ""
	s.Refresh()
	if _, ok := s.table.Load().Route("app.tailtest.ts.net"); !ok {
		t.Fatal("fallback table missing app.tailtest.ts.net")
	}
	if got := st.stamped["app"]; got != "https://app.tailtest.ts.net" {
		t.Fatalf("fallback url = %q", got)
	}
	if started["app"] == nil || started["app"].closed {
		t.Fatalf("fallback node not running: %v", started)
	}
	if got := s.CertStatus(); got != "unset" {
		t.Fatalf("fallback cert status = %q", got)
	}

	// Back to domain mode: the node is stopped.
	st.domain = "dev.test"
	s.Refresh()
	if !started["app"].closed {
		t.Fatal("node kept running in domain mode")
	}
}

func TestReconcileNodes_RemoveAndKeyChange(t *testing.T) {
	st := &fakeState{hostnames: map[string]int{"app": 1, "api": 2}}
	s, started := testServer(t, st)
	s.Refresh()
	first := started["app"]

	// Key change restarts nodes (unsticks a node waiting at a login URL).
	st.authKey = "tskey-new"
	s.Refresh()
	if !first.closed || started["app"] == first || started["app"].closed {
		t.Fatal("key change did not restart the node")
	}

	// Removing a mapping stops its node; the survivor keeps running.
	delete(st.hostnames, "api")
	s.Refresh()
	if !started["api"].closed || started["app"].closed {
		t.Fatalf("remove reconcile wrong: api=%+v app=%+v", started["api"], started["app"])
	}
}

// freePort grabs a port that is free right now (and closed by the time the
// test uses it, which is exactly what the backend-down cases want).
func freePort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	ln.Close()
	return port
}
