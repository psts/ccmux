package api

import (
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gorilla/websocket"

	"ccmux.dev/ccmuxd/internal/peers"
	"ccmux.dev/ccmuxd/internal/store"
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

// viewerRelayServer is relayServer with the peers bus enabled, which the lens
// half of the relay needs: the credential it checks is this host's own. The
// upstream mapper returns a token on purpose, so a test can prove a viewer read
// does NOT get it.
func viewerRelayServer(t *testing.T, hub http.Handler) (member *httptest.Server, token string) {
	t.Helper()
	handler, token := viewerRelayHandler(t, hub)
	member = httptest.NewServer(handler)
	t.Cleanup(member.Close)
	return member, token
}

// viewerRelayHandler is the same wiring unserved, for tests that need to forge a
// RemoteAddr — a real loopback listener can only ever report 127.0.0.1, and the
// origin rule is exactly what those tests are about.
func viewerRelayHandler(t *testing.T, hub http.Handler) (http.Handler, string) {
	t.Helper()
	hubSrv := httptest.NewServer(hub)
	t.Cleanup(hubSrv.Close)

	st, err := store.Open(filepath.Join(t.TempDir(), "peers.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	svc := peers.NewService(st, nullHook{}, testSecret)
	svc.OpenCmd = ""

	s := &Server{peersSvc: svc, upgrader: websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}}
	// Faithful to production: only this host's pane-less credential is swapped
	// for the hub's, everything else passes through for the hub to judge.
	s.SetHubBus(func() string { return hubSrv.URL }, http.DefaultTransport, func(inbound string) (string, error) {
		if inbound != peers.PanelessToken(testSecret) {
			return "", nil
		}
		return "hub-secret", nil
	})
	return s.Handler(), peers.PanelessToken(testSecret)
}

// A lens read must cross to the hub, or a member host's overlay shows an empty
// panel while every session it is asking about sits on the hub's bus.
func TestHubBusRelay_ViewerReadCrossesWithTheLocalToken(t *testing.T) {
	var gotPath, gotQuery, gotAuth string
	member, token := viewerRelayServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotQuery, gotAuth = r.URL.Path, r.URL.RawQuery, r.Header.Get("Authorization")
		writeJSON(w, http.StatusOK, []map[string]string{{"id": "peer-1", "group": "ChartLabs"}})
	}))

	req, _ := http.NewRequest("GET", member.URL+HubBusPrefix+"/v1/peers?group=ChartLabs", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)

	if resp.StatusCode != 200 {
		t.Fatalf("status = %d, want 200 (body %s)", resp.StatusCode, body)
	}
	if gotPath != "/v1/peers" || gotQuery != "group=ChartLabs" {
		t.Errorf("hub saw %s?%s, want /v1/peers?group=ChartLabs", gotPath, gotQuery)
	}
	if gotAuth != "" {
		t.Errorf("hub saw auth %q — a local secret must not ride onto the tailnet", gotAuth)
	}
	if !strings.Contains(string(body), "peer-1") {
		t.Errorf("hub response not returned to the caller: %s", body)
	}
}

// The token is what replaces the tailnet boundary the hub's own viewer surface
// relies on. Without it the relay would hand every local account on a shared
// host a fleet-wide read.
func TestHubBusRelay_ViewerReadNeedsTheToken(t *testing.T) {
	reached := false
	member, token := viewerRelayServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reached = true
	}))

	for _, tc := range []struct{ name, bearer string }{
		{"no token", ""},
		{"wrong token", "not-the-local-secret"},
		{"a pane token, which is not the lens credential", peers.TokenForPane(testSecret, "pane-7")},
	} {
		req, _ := http.NewRequest("GET", member.URL+HubBusPrefix+"/v1/peers/messages?group=ChartLabs", nil)
		if tc.bearer != "" {
			req.Header.Set("Authorization", "Bearer "+tc.bearer)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("%s: status = %d, want 401", tc.name, resp.StatusCode)
		}
	}
	if reached {
		t.Error("an unauthorized read reached the hub")
	}
	// The same request with the real credential proves the refusals above were
	// about the token and not about the route.
	req, _ := http.NewRequest("GET", member.URL+HubBusPrefix+"/v1/peers/messages?group=ChartLabs", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Errorf("status = %d with the local token, want 200", resp.StatusCode)
	}
}

// A browser's WebSocket constructor cannot set headers, so the listen stream —
// the one viewer surface that must be a socket — takes its credential in the
// query string. It must not travel any further than this daemon.
func TestHubBusRelay_ListenSocketTakesAQueryToken(t *testing.T) {
	up := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	var gotQuery, gotAuth string
	member, token := viewerRelayServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotQuery, gotAuth = r.URL.RawQuery, r.Header.Get("Authorization")
		c, err := up.Upgrade(w, r, nil)
		if err != nil {
			t.Errorf("hub upgrade: %v", err)
			return
		}
		defer c.Close()
		_ = c.WriteMessage(websocket.TextMessage, []byte(`{"type":"message"}`))
	}))

	wsURL := "ws" + strings.TrimPrefix(member.URL, "http") + HubBusPrefix +
		"/v1/peers/ws?mode=listen&group=ChartLabs&" + ViewerTokenParam + "=" + token
	conn, resp, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		code := 0
		if resp != nil {
			code = resp.StatusCode
		}
		t.Fatalf("listen dial through relay: %v (HTTP %d)", err, code)
	}
	defer conn.Close()
	if _, msg, err := conn.ReadMessage(); err != nil {
		t.Fatalf("read through relay: %v", err)
	} else if string(msg) != `{"type":"message"}` {
		t.Errorf("got %q, want the hub's push", msg)
	}
	if strings.Contains(gotQuery, ViewerTokenParam) {
		t.Errorf("hub saw %q — the local token must be stripped at the relay", gotQuery)
	}
	if gotAuth != "" {
		t.Errorf("hub saw auth %q on a viewer socket", gotAuth)
	}
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

func TestRelayKindFor(t *testing.T) {
	for _, tc := range []struct {
		path string
		want relayKind
	}{
		{"/v1/peers/register", relayClient},
		{"/v1/peers/unregister", relayClient},
		{"/v1/peers/send", relayClient},
		{"/v1/peers/list", relayClient},
		{"/v1/peers/summary", relayClient},
		{"/v1/peers/poll", relayClient},
		{"/v1/peers/permission-request", relayClient},
		{"/v1/peers/tasks/delegate", relayClient},
		{"/v1/peers/tasks/update", relayClient},
		{"/v1/peers/tasks/list", relayClient},
		{"/v1/peers/ws", relayClient},
		{"/v1/peers/bus", relayDenied},
		{"/v1/peers/pane-token", relayDenied},
		{"/v1/peers/local-groups", relayDenied},
		// The lens read surface: relayable, but under this host's own token
		// rather than a bus credential.
		{"/v1/peers/messages", relayViewer},
		{"/v1/peers", relayViewer},
		// The allowlist's whole point — a route nobody has written yet is not
		// relayable by default.
		{"/v1/peers/some-future-route", relayDenied},
		{"/v1/peersies", relayDenied},
		{"/v1/health", relayDenied},
		{"/", relayDenied},
	} {
		req := httptest.NewRequest("POST", tc.path, nil)
		if got := relayKindFor(req); got != tc.want {
			t.Errorf("relayKindFor(%q) = %v, want %v", tc.path, got, tc.want)
		}
	}
}

// The socket is two handlers behind one path, and which one you get decides
// which credential answers for it. mode= that is neither is refused outright:
// the thin client never sends it, so the form has no legitimate caller.
func TestRelayKindFor_SocketArms(t *testing.T) {
	for _, tc := range []struct {
		query string
		want  relayKind
	}{
		{"peer_id=peer-1", relayClient},
		{"mode=listen&group=ChartLabs", relayViewer},
		{"mode=peer&peer_id=peer-1", relayDenied},
		{"mode=whatever", relayDenied},
	} {
		req := httptest.NewRequest("GET", "/v1/peers/ws?"+tc.query, nil)
		if got := relayKindFor(req); got != tc.want {
			t.Errorf("relayKindFor(ws?%s) = %v, want %v", tc.query, got, tc.want)
		}
	}
}
