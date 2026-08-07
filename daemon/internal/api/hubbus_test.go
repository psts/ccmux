package api

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gorilla/websocket"
)

// relayServer wires a member-side relay pointing at a stub hub, and returns the
// member's routed handler served on loopback plus the stub. hubOf lets a test
// move or unset the hub between requests, which is the state hub discovery
// actually produces.
func relayServer(t *testing.T, hub http.Handler) (member *httptest.Server, hubSrv *httptest.Server, setHub func(string)) {
	t.Helper()
	hubSrv = httptest.NewServer(hub)
	t.Cleanup(hubSrv.Close)

	target := hubSrv.URL
	s := &Server{}
	s.SetHubBus(func() string { return target }, http.DefaultTransport, nil)
	member = httptest.NewServer(s.Handler())
	t.Cleanup(member.Close)
	return member, hubSrv, func(u string) { target = u }
}

// hostOf is the authority of a test server's URL ("127.0.0.1:PORT"). Both the
// member and the stub hub listen on loopback, so only the PORT distinguishes
// them — a Host assertion has to compare the whole authority.
func hostOf(t *testing.T, srv *httptest.Server) string {
	t.Helper()
	return strings.TrimPrefix(srv.URL, "http://")
}

// TestHubBusRelay_ForwardsStripped is the contract the thin client depends on:
// it appends the SAME /v1/peers/… paths it would have sent to the hub, so the
// prefix must come off and everything else must survive the hop.
func TestHubBusRelay_ForwardsStripped(t *testing.T) {
	var gotPath, gotAuth, gotBody, gotHost string
	member, hub, _ := relayServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotAuth, gotHost = r.URL.Path, r.Header.Get("Authorization"), r.Host
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		writeJSON(w, http.StatusOK, map[string]string{"id": "peer-1"})
	}))

	req, err := http.NewRequest("POST", member.URL+HubBusPrefix+"/v1/peers/register",
		strings.NewReader(`{"pane_id":"pane-7"}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer hub-token")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if gotPath != "/v1/peers/register" {
		t.Errorf("hub saw path %q, want /v1/peers/register (prefix must be stripped)", gotPath)
	}
	if gotAuth != "Bearer hub-token" {
		t.Errorf("hub saw auth %q — the hub-minted token must reach it", gotAuth)
	}
	if gotBody != `{"pane_id":"pane-7"}` {
		t.Errorf("hub saw body %q", gotBody)
	}
	if gotHost != hostOf(t, hub) {
		t.Errorf("hub saw Host %q, want %q — a proxied request must look addressed to the hub, not to the pane's loopback (%s)",
			gotHost, hostOf(t, hub), hostOf(t, member))
	}
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "peer-1") {
		t.Errorf("hub response not returned to the caller: %s", body)
	}
}

// TestHubBusRelay_WebSocket is the one that earns the reverse proxy: the bus
// inbox is a long-lived socket, and a relay that carried only request/response
// would leave every pane push-blind while looking healthy.
func TestHubBusRelay_WebSocket(t *testing.T) {
	up := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	member, _, _ := relayServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/peers/ws" || r.URL.Query().Get("peer_id") != "peer-1" {
			t.Errorf("hub saw %s?%s", r.URL.Path, r.URL.RawQuery)
		}
		c, err := up.Upgrade(w, r, nil)
		if err != nil {
			t.Errorf("hub upgrade: %v", err)
			return
		}
		defer c.Close()
		_ = c.WriteMessage(websocket.TextMessage, []byte(`{"event":"hello"}`))
	}))

	wsURL := "ws" + strings.TrimPrefix(member.URL, "http") + HubBusPrefix + "/v1/peers/ws?peer_id=peer-1"
	conn, resp, err := websocket.DefaultDialer.Dial(wsURL, http.Header{"Authorization": {"Bearer t"}})
	if err != nil {
		code := 0
		if resp != nil {
			code = resp.StatusCode
		}
		t.Fatalf("dial through relay: %v (HTTP %d)", err, code)
	}
	defer conn.Close()
	_, msg, err := conn.ReadMessage()
	if err != nil {
		t.Fatalf("read through relay: %v", err)
	}
	if string(msg) != `{"event":"hello"}` {
		t.Errorf("got %q, want the hub's push", msg)
	}
}

// TestHubBusRelay_NoHub: "no hub right now" must be a retryable 503, not a 404 —
// a 404 is how the thin client recognizes an older daemon with no route at all.
func TestHubBusRelay_NoHub(t *testing.T) {
	member, _, setHub := relayServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	setHub("")
	resp, err := http.Post(member.URL+HubBusPrefix+"/v1/peers/list", "application/json", strings.NewReader("{}"))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", resp.StatusCode)
	}
}

// TestHubBusRelay_FollowsHubMove: the target is read per request because hub
// discovery re-resolves the tag every 15s. Caching it at wiring time would pin a
// member to a hub that has moved until the daemon restarted.
func TestHubBusRelay_FollowsHubMove(t *testing.T) {
	member, _, setHub := relayServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"hub": "first"})
	}))
	second := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"hub": "second"})
	}))
	defer second.Close()

	ask := func() string {
		resp, err := http.Post(member.URL+HubBusPrefix+"/v1/peers/list", "application/json", strings.NewReader("{}"))
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return string(b)
	}
	if got := ask(); !strings.Contains(got, "first") {
		t.Fatalf("first answer = %s", got)
	}
	setHub(second.URL)
	if got := ask(); !strings.Contains(got, "second") {
		t.Errorf("after the hub moved, answer = %s — the relay pinned the old target", got)
	}
}

// TestHubBusRelay_PathsNotRelayable: the relay is not a general tunnel. Anything
// outside the bus paths, and the hub-authority routes in particular, must stop
// here — /v1/peers/pane-token mints a credential for an ARBITRARY pane id, and
// the hop would present this host's member identity for it.
func TestHubBusRelay_PathsNotRelayable(t *testing.T) {
	reached := false
	member, _, _ := relayServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reached = true
	}))
	for _, path := range []string{
		"/v1/peers/pane-token",
		"/v1/peers/bus",
		"/v1/peers/local-groups",
		"/v1/workspaces",
		"/v1/hosts",
		"/v1/health",
	} {
		resp, err := http.Post(member.URL+HubBusPrefix+path, "application/json", strings.NewReader("{}"))
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusForbidden {
			t.Errorf("%s: status = %d, want 403", path, resp.StatusCode)
		}
	}
	if reached {
		t.Error("a blocked path reached the hub")
	}
}

// TestHubBusRelay_LoopbackOnly: the member's handler is served on the tailnet
// too, so an unguarded relay would let any tailnet node reach the hub's bus
// through this host, wearing this host's member identity.
func TestHubBusRelay_LoopbackOnly(t *testing.T) {
	s := &Server{}
	s.SetHubBus(func() string { return "https://hub.ts.net" }, http.DefaultTransport, nil)
	req := httptest.NewRequest("POST", HubBusPrefix+"/v1/peers/register", strings.NewReader("{}"))
	req.RemoteAddr = "100.64.0.9:41000"
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 for a tailnet caller", rec.Code)
	}
}

// TestHubBusRelay_UnarmedHasNoRoute: on a hub or a single-host node the prefix
// must not exist at all, rather than answering 503 forever.
func TestHubBusRelay_UnarmedHasNoRoute(t *testing.T) {
	s := &Server{}
	req := httptest.NewRequest("POST", HubBusPrefix+"/v1/peers/register", strings.NewReader("{}"))
	req.RemoteAddr = "127.0.0.1:5000"
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	if rec.Code == http.StatusServiceUnavailable || rec.Code == http.StatusOK {
		t.Fatalf("status = %d, want a not-routed answer with no relay armed", rec.Code)
	}
}

func TestRelayable(t *testing.T) {
	for _, tc := range []struct {
		path string
		want bool
	}{
		{"/v1/peers/register", true},
		{"/v1/peers/unregister", true},
		{"/v1/peers/send", true},
		{"/v1/peers/list", true},
		{"/v1/peers/summary", true},
		{"/v1/peers/poll", true},
		{"/v1/peers/permission-request", true},
		{"/v1/peers/tasks/delegate", true},
		{"/v1/peers/tasks/update", true},
		{"/v1/peers/tasks/list", true},
		{"/v1/peers/ws", true},
		{"/v1/peers/bus", false},
		{"/v1/peers/pane-token", false},
		{"/v1/peers/local-groups", false},
		// Viewer routes: a lens reads them from the hub directly, and a pane's
		// client never sends them.
		{"/v1/peers/messages", false},
		{"/v1/peers", false},
		// The allowlist's whole point — a route nobody has written yet is not
		// relayable by default.
		{"/v1/peers/some-future-route", false},
		{"/v1/peersies", false},
		{"/v1/health", false},
		{"/", false},
	} {
		req := httptest.NewRequest("POST", tc.path, nil)
		if got := relayable(req); got != tc.want {
			t.Errorf("relayable(%q) = %v, want %v", tc.path, got, tc.want)
		}
	}
}
