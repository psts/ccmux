package api

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"ccmux.dev/ccmuxd/internal/harness"
	"ccmux.dev/ccmuxd/internal/manager"
	"ccmux.dev/ccmuxd/internal/peers"
	"ccmux.dev/ccmuxd/internal/store"
	"ccmux.dev/ccmuxd/internal/tmux"
	"ccmux.dev/ccmuxd/internal/walls"
)

const (
	gateToken = "the-api-token"
	fromHere  = "127.0.0.1:50000"
	fromAway  = "100.64.0.9:50000" // another machine on the tailnet
)

// gatedServer is a daemon's API with the gate on: a bus, a sandbox tab,
// and this host's own addresses fixed (its tailnet address is 100.99.0.1).
func gatedServer(t *testing.T) *Server {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "reg.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	mgr := manager.New(context.Background(), &tmux.Server{Socket: "unused"}, st)
	mgr.Harnesses = harness.New(st)
	s := NewServer(mgr)
	s.peersSvc = peers.NewService(st, mgr, testSecret)
	s.SetSandbox(&fakeSandbox{host: walls.Host{Caps: true, Walls: true}})
	s.SetAPIToken(gateToken)
	s.own.addrs = func() ([]net.Addr, error) {
		return []net.Addr{&net.IPNet{IP: net.ParseIP("100.99.0.1"), Mask: net.CIDRMask(32, 32)}, &net.IPNet{IP: net.ParseIP("192.168.1.130"), Mask: net.CIDRMask(24, 32)}}, nil
	}
	return s
}

// call is one request as the gate sees it; with sets what the caller
// sends.
func gateCall(s *Server, method, target, from string, with func(*http.Request)) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, target, strings.NewReader("{}"))
	req.RemoteAddr = from
	req.Host = "127.0.0.1:7900"
	if with != nil {
		with(req)
	}
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	return rec
}

func withBearer(token string) func(*http.Request) {
	return func(r *http.Request) { r.Header.Set("Authorization", "Bearer "+token) }
}

// refusedByGate reports whether an answer is the gate's own refusal, not a
// handler's.
func refusedByGate(rec *httptest.ResponseRecorder) bool {
	var body struct{ Code string }
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	return rec.Code == http.StatusUnauthorized && body.Code == "local-key"
}

// Every way out of the walls the review found, and the rest of the API
// with them: from this machine each needs the token.
var gatedRoutes = []string{
	"PUT /v1/sandbox", "GET /v1/sandbox", "POST /v1/sandbox/check",
	"POST /v1/sessions", "GET /v1/sessions", "POST /v1/sessions/s1/prompt", "POST /v1/sessions/s1/wanted",
	"POST /v1/sessions/s1/restart", "POST /v1/sessions/s1/model", "POST /v1/sessions/s1/cards/c1", "GET /v1/sessions/s1/ws",
	"POST /v1/threads", "POST /v1/threads/1/merge", "POST /v1/threads/1/end",
	"PATCH /v1/tasks/1", "POST /v1/tasks/1/move", "DELETE /v1/tasks/1", "POST /v1/todos/1/task",
	"PUT /v1/agents/radar", "DELETE /v1/agents/radar", "POST /v1/agents/radar/mcp", "POST /v1/agents/radar/instances/restart",
	"POST /v1/windows/w1/agents/radar", "DELETE /v1/windows/w1/agents/radar",
	"POST /v1/workspaces", "POST /v1/workspaces/w1/panes", "PUT /v1/workspaces/w1/files", "GET /v1/workspaces/w1/files", "POST /v1/workspaces/w1/revive",
	"POST /v1/panes/p1/harness", "POST /v1/panes/p1/agent/reply", "GET /v1/panes/p1/agent/ws", "PUT /v1/panes/p1/llm-route",
	"GET /v1/attach", "GET /v1/events", "PUT /v1/settings", "GET /v1/settings", "POST /v1/upgrade", "POST /v1/projects",
	"POST /v1/capabilities/import", "PUT /v1/windows/w1/slots",
	"GET /v1/llm/accounts/main/key",
	// Bus routes a session never calls: each hands out a token or a view.
	"POST /v1/peers/pane-token", "POST /v1/peers/host-token", "PUT /v1/peers/local-groups", "GET /v1/peers/viewer", "GET /v1/peers/messages", "GET /v1/peers",
	// A route nobody has written yet is gated too.
	"POST /v1/something-new", "GET /v1/something-new/x",
}

func TestLocalGate_FromThisMachineEveryRouteNeedsTheToken(t *testing.T) {
	s := gatedServer(t)
	for _, route := range gatedRoutes {
		method, target, _ := strings.Cut(route, " ")
		if rec := gateCall(s, method, target, fromHere, nil); !refusedByGate(rec) {
			t.Errorf("%s with no token: want the gate's refusal, got %d %s", route, rec.Code, rec.Body.String())
		}
		if rec := gateCall(s, method, target, fromHere, withBearer("not-the-token")); !refusedByGate(rec) {
			t.Errorf("%s with a wrong token: want the gate's refusal, got %d", route, rec.Code)
		}
		// A pane's own token is not the API token.
		if rec := gateCall(s, method, target, fromHere, withBearer(s.peersSvc.MintPaneToken("p1"))); !refusedByGate(rec) {
			t.Errorf("%s with a pane's token: want the gate's refusal, got %d", route, rec.Code)
		}
		if rec := gateCall(s, method, target, fromHere, withBearer(gateToken)); refusedByGate(rec) {
			t.Errorf("%s with the token: the gate must let it through", route)
		}
	}
}

func TestLocalGate_TheTokenIsTakenAsBearerHeaderOrCookie(t *testing.T) {
	s := gatedServer(t)
	for name, with := range map[string]func(*http.Request){
		"bearer": withBearer(gateToken),
		"header": func(r *http.Request) { r.Header.Set("X-Ccmux-Key", gateToken) },
		"cookie": func(r *http.Request) { r.AddCookie(&http.Cookie{Name: "ccmux_key_7900", Value: gateToken}) },
	} {
		if rec := gateCall(s, "GET", "/v1/sandbox", fromHere, with); rec.Code != http.StatusOK {
			t.Errorf("%s: %d %s", name, rec.Code, rec.Body.String())
		}
	}
	// Another daemon's cookie (another port's) is not this one's.
	other := func(r *http.Request) { r.AddCookie(&http.Cookie{Name: "ccmux_key_7911", Value: gateToken}) }
	if rec := gateCall(s, "GET", "/v1/sandbox", fromHere, other); !refusedByGate(rec) {
		t.Errorf("a cookie named for another port: %d", rec.Code)
	}
}

// The escape itself: a walled session's curl.
func TestLocalGate_AWalledSessionCannotSwitchTheSandboxOff(t *testing.T) {
	s := gatedServer(t)
	if err := s.mgr.SetSandbox(walls.Settings{On: true, MemoryPercent: 75, CPUCores: 1, TasksMax: 8192, JobHours: 6}); err != nil {
		t.Fatal(err)
	}
	off := func(r *http.Request) {
		r.Body = httptest.NewRequest("PUT", "/", strings.NewReader(`{"on":false,"memoryPercent":75,"cpuCores":1,"tasksMax":8192,"jobHours":6}`)).Body
	}
	if rec := gateCall(s, "PUT", "/v1/sandbox", fromHere, off); !refusedByGate(rec) {
		t.Fatalf("PUT /v1/sandbox from this machine with no token: %d %s", rec.Code, rec.Body.String())
	}
	if set, err := s.mgr.Sandbox(); err != nil || !set.On {
		t.Fatalf("the sandbox must still be on: %+v %v", set, err)
	}
	// With its own pane's token, and through its own tailnet address, neither.
	own := s.peersSvc.MintPaneToken("p1")
	for _, from := range []string{fromHere, "100.99.0.1:40000", "192.168.1.130:40000", "[::1]:40000"} {
		rec := gateCall(s, "PUT", "/v1/sandbox", from, func(r *http.Request) { off(r); withBearer(own)(r) })
		if !refusedByGate(rec) {
			t.Errorf("from %s: %d", from, rec.Code)
		}
	}
	if set, _ := s.mgr.Sandbox(); !set.On {
		t.Fatal("the sandbox was switched off")
	}
}

func TestLocalGate_AnotherMachineIsNotAsked(t *testing.T) {
	s := gatedServer(t)
	if rec := gateCall(s, "GET", "/v1/sandbox", fromAway, nil); rec.Code != http.StatusOK {
		t.Fatalf("a caller the tailnet brought in needs no token: %d %s", rec.Code, rec.Body.String())
	}
	if rec := gateCall(s, "GET", "/", fromAway, nil); rec.Code == http.StatusUnauthorized {
		t.Fatal("nor is its lens page replaced by the key page")
	}
}

func TestLocalGate_AServerWithNoTokenGatesNothing(t *testing.T) {
	s := gatedServer(t)
	s.SetAPIToken("")
	if rec := gateCall(s, "GET", "/v1/sandbox", fromHere, nil); rec.Code != http.StatusOK {
		t.Fatalf("no token set, no gate: %d", rec.Code)
	}
	var health map[string]any
	json.Unmarshal(gateCall(s, "GET", "/v1/health", fromHere, nil).Body.Bytes(), &health)
	if health["localGate"] != false {
		t.Fatalf("and health says so: %v", health["localGate"])
	}
	s.SetAPIToken(gateToken)
	json.Unmarshal(gateCall(s, "GET", "/v1/health", fromHere, nil).Body.Bytes(), &health)
	if health["localGate"] != true {
		t.Fatalf("health says the gate is on: %v", health["localGate"])
	}
}

// What a session calls stays open to it, and each of those routes knows
// its caller by a token of its own: open never means anyone.
func TestLocalGate_TheBusStaysOpenAndStillWantsItsOwnToken(t *testing.T) {
	s := gatedServer(t)
	for _, open := range []string{"GET /v1/health", "GET /v1/whoami"} {
		method, target, _ := strings.Cut(open, " ")
		if rec := gateCall(s, method, target, fromHere, nil); rec.Code != http.StatusOK {
			t.Errorf("%s: %d", open, rec.Code)
		}
	}
	for route := range localOpen {
		method, target, _ := strings.Cut(route, " ")
		if !strings.HasPrefix(target, "/v1/peers/") && target != "/v1/clipboard" {
			continue
		}
		body := func(r *http.Request) {
			r.Body = httptest.NewRequest("POST", "/", strings.NewReader(`{"peer_id":"pane:p9","from_id":"pane:p9","pane_id":"p9","to_id":"pane:p1","id":"pane:p9"}`)).Body
			if method == "GET" {
				r.URL.RawQuery = "peer_id=pane:p9"
			}
		}
		rec := gateCall(s, method, target, fromHere, body)
		if refusedByGate(rec) {
			t.Errorf("%s: the gate must leave it to the route", route)
		}
		if rec.Code < 400 {
			t.Errorf("%s with no token of any kind answered %d: an open route must still know its caller", route, rec.Code)
		}
	}
}

// The bus socket has two modes behind one path. The one a session's shim
// uses checks the peer's token itself and stays open; the other streams a
// whole group's messages to whoever asks, which is a lens's view.
func TestLocalGate_TheBusSocketsListenModeNeedsTheToken(t *testing.T) {
	s := gatedServer(t)
	own := s.peersSvc.MintPaneToken("p1")
	for _, target := range []string{"/v1/peers/ws?mode=listen&group=t183", "/v1/peers/ws?group=t183&mode=listen", "/v1/peers/ws?mode=listen"} {
		if rec := gateCall(s, "GET", target, fromHere, nil); !refusedByGate(rec) {
			t.Errorf("%s with no token: want the gate's refusal, got %d %s", target, rec.Code, rec.Body.String())
		}
		if rec := gateCall(s, "GET", target, fromHere, withBearer(own)); !refusedByGate(rec) {
			t.Errorf("%s with a pane's token: want the gate's refusal, got %d", target, rec.Code)
		}
		if rec := gateCall(s, "GET", target, fromHere, withBearer(gateToken)); refusedByGate(rec) {
			t.Errorf("%s with the API token: the gate must let the lens through", target)
		}
		if rec := gateCall(s, "GET", target, fromAway, nil); refusedByGate(rec) {
			t.Errorf("%s from another machine is not asked", target)
		}
	}
	// The shim's mode is left to the route, which wants the peer's token.
	for _, target := range []string{"/v1/peers/ws?peer_id=pane:p1", "/v1/peers/ws?mode=peer&peer_id=pane:p1", "/v1/peers/ws?peer_id=pane:p1&group=listen"} {
		rec := gateCall(s, "GET", target, fromHere, nil)
		if refusedByGate(rec) {
			t.Errorf("%s: the gate must leave the peer mode to the route", target)
		}
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("%s with no token: the route itself refuses, got %d", target, rec.Code)
		}
	}
}

func TestLocalGate_APanesToolsWantThatPanesOwnToken(t *testing.T) {
	s := gatedServer(t)
	own, other := s.peersSvc.MintPaneToken("p1"), s.peersSvc.MintPaneToken("p2")
	for route := range paneOwn {
		method, tail, _ := strings.Cut(route, " ")
		target := "/v1/panes/p1/" + tail
		if rec := gateCall(s, method, target, fromHere, nil); !refusedByGate(rec) {
			t.Errorf("%s with no token: %d", target, rec.Code)
		}
		if rec := gateCall(s, method, target, fromHere, withBearer(other)); !refusedByGate(rec) {
			t.Errorf("%s with another pane's token: %d", target, rec.Code)
		}
		if rec := gateCall(s, method, target, fromHere, withBearer(own)); refusedByGate(rec) {
			t.Errorf("%s with the pane's own token: the gate must let it through", target)
		}
	}
	// What a plugin and the co-author git hook call with no token: a mark,
	// and two reads that start nothing.
	for _, route := range []string{"POST agent-signal", "GET bus-context", "GET driver"} {
		method, tail, _ := strings.Cut(route, " ")
		if rec := gateCall(s, method, "/v1/panes/p1/"+tail, fromHere, nil); refusedByGate(rec) {
			t.Errorf("%s: the gate must leave it open", route)
		}
	}
	// The same names under another method, and a pane's other reads, are
	// the lens's: gated.
	for _, route := range []string{"POST driver", "PUT driver", "GET agent-signal", "POST bus-context", "GET snapshot", "GET agent", "GET llm-route", "GET driver/x"} {
		method, tail, _ := strings.Cut(route, " ")
		if rec := gateCall(s, method, "/v1/panes/p1/"+tail, fromHere, nil); !refusedByGate(rec) {
			t.Errorf("%s: want the gate's refusal, got %d", route, rec.Code)
		}
	}
	// A pane route that is on neither list is the lens's, and gated.
	if rec := gateCall(s, "POST", "/v1/panes/p1/resize", fromHere, withBearer(own)); !refusedByGate(rec) {
		t.Errorf("a pane's token opens only that pane's own tools: %d", rec.Code)
	}
}

// A path that reads as an open route once decoded and cleaned, and that
// the mux takes for another route, is not open.
func TestLocalGate_APathDressedAsAnOpenRouteIsNotOpen(t *testing.T) {
	s := gatedServer(t)
	for _, target := range []string{
		"/v1/windows/w1/agents/..%2F..%2F..%2Fhealth",
		"/v1/sessions/..%2Fhealth",
		"/v1/panes/p1/../../sandbox",
		"/v1/sandbox/../health/../sandbox",
		"//v1/sandbox",
		"/v1/health/../sandbox",
		"/v1/panes/p1%2Fagent-signal",
	} {
		for _, method := range []string{"GET", "POST", "PUT"} {
			req := httptest.NewRequest(method, "http://127.0.0.1:7900"+target, strings.NewReader("{}"))
			req.RemoteAddr = fromHere
			rec := httptest.NewRecorder()
			s.Handler().ServeHTTP(rec, req)
			if !refusedByGate(rec) {
				t.Errorf("%s %s: want the gate's refusal, got %d %s", method, target, rec.Code, rec.Body.String())
			}
		}
	}
}

func TestLocalGate_AKeyLinkBecomesACookieUnderTheLensesOwnName(t *testing.T) {
	s := gatedServer(t)
	// Opened under a plain loopback name: sent on to the lens's own name
	// first, where the cookie is no dev server's to receive.
	rec := gateCall(s, "GET", "/threads.html?key="+gateToken+"&s=abc", fromHere, nil)
	if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "http://ccmux.localhost:7900/threads.html?key="+gateToken+"&s=abc" || len(rec.Result().Cookies()) != 0 {
		t.Fatalf("under 127.0.0.1: %d %q %v", rec.Code, rec.Header().Get("Location"), rec.Result().Cookies())
	}
	lens := func(r *http.Request) { r.Host = "ccmux.localhost:7900" }
	rec = gateCall(s, "GET", "/threads.html?key="+gateToken+"&s=abc", fromHere, lens)
	cookies := rec.Result().Cookies()
	if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/threads.html?s=abc" || len(cookies) != 1 {
		t.Fatalf("under the lens's name: %d %q %v", rec.Code, rec.Header().Get("Location"), cookies)
	}
	c := cookies[0]
	if c.Name != "ccmux_key_7900" || c.Value != gateToken || !c.HttpOnly || c.SameSite != http.SameSiteStrictMode || c.Path != "/" {
		t.Fatalf("the cookie: %+v", c)
	}
	withCookie := func(r *http.Request) { lens(r); r.AddCookie(c) }
	if rec := gateCall(s, "GET", "/v1/sandbox", fromHere, withCookie); rec.Code != http.StatusOK {
		t.Fatalf("the cookie opens the API: %d", rec.Code)
	}
	if rec := gateCall(s, "GET", "/", fromHere, withCookie); rec.Code != http.StatusOK || strings.Contains(rec.Body.String(), "needs the daemon's key") {
		t.Fatalf("and the lens loads: %d", rec.Code)
	}
	// A wrong key says what to do, and sets nothing.
	rec = gateCall(s, "GET", "/?key=stale", fromHere, lens)
	if rec.Code != http.StatusUnauthorized || !strings.Contains(rec.Body.String(), "ccmuxd lens-url") || len(rec.Result().Cookies()) != 0 {
		t.Fatalf("a stale link: %d %s", rec.Code, rec.Body.String())
	}
	// "key" is only a key link on a page's address. Anywhere else it is the
	// route's own field: the gate neither answers for the route nor takes a
	// right key there for a token.
	for _, target := range []string{"/v1/health?key=stale", "/v1/health?key=" + gateToken, "/app.js?key=stale", "/llm/pane/p1/v1/models?key=an-upstream-api-key"} {
		rec := gateCall(s, "GET", target, fromHere, lens)
		if rec.Code == http.StatusSeeOther || strings.Contains(rec.Body.String(), "that link's key") || len(rec.Result().Cookies()) != 0 {
			t.Errorf("%s was taken for a key link: %d %s", target, rec.Code, rec.Body.String())
		}
	}
	if rec := gateCall(s, "GET", "/v1/sandbox?key="+gateToken, fromHere, lens); !refusedByGate(rec) {
		t.Errorf("the token in an API call's query is not a way in: %d", rec.Code)
	}
	// From another machine the field is nobody's business: nothing is set.
	if rec := gateCall(s, "GET", "/?key="+gateToken, fromAway, nil); len(rec.Result().Cookies()) != 0 || rec.Code == http.StatusSeeOther {
		t.Fatalf("a caller the gate does not ask is not given a cookie: %d", rec.Code)
	}
}

func TestLocalGate_APageOpenedHereWithoutTheKeySaysHowToGetIt(t *testing.T) {
	s := gatedServer(t)
	for _, page := range []string{"/", "/index.html", "/threads.html"} {
		rec := gateCall(s, "GET", page, fromHere, nil)
		if rec.Code != http.StatusUnauthorized || !strings.Contains(rec.Body.String(), "ccmuxd lens-url") {
			t.Errorf("%s: %d %s", page, rec.Code, rec.Body.String())
		}
	}
	// The lens's files hold no secret and stay open: a page that has the
	// key must not find its scripts refused.
	if rec := gateCall(s, "GET", "/app.js", fromHere, nil); rec.Code != http.StatusOK {
		t.Errorf("/app.js: %d", rec.Code)
	}
}

func TestOwnAddrs_ThisMachineIsLoopbackAndItsOwnInterfaces(t *testing.T) {
	now := time.Unix(1000, 0)
	reads, fail := 0, false
	list := []net.Addr{&net.IPNet{IP: net.ParseIP("100.99.0.1"), Mask: net.CIDRMask(32, 32)}, &net.IPNet{IP: net.ParseIP("fd7a::1"), Mask: net.CIDRMask(128, 128)}}
	o := &ownAddrs{now: func() time.Time { return now }, addrs: func() ([]net.Addr, error) {
		reads++
		if fail {
			return nil, errors.New("netlink: no")
		}
		return list, nil
	}}
	for addr, want := range map[string]bool{
		"127.0.0.1:1": true, "[::1]:1": true, "127.8.8.8:1": true,
		"100.99.0.1:1": true, "[fd7a::1]:1": true, "[::ffff:100.99.0.1]:1": true,
		"100.64.0.9:1": false, "[fd7a::2]:1": false, "192.168.1.130:1": false,
		"not-an-address": true, "": true,
	} {
		if got := o.local(addr); got != want {
			t.Errorf("local(%q) = %v, want %v", addr, got, want)
		}
	}
	if reads != 1 {
		t.Fatalf("the list is read once while it is fresh: %d", reads)
	}
	// A new interface is seen once the list is old.
	list = append(list, &net.IPNet{IP: net.ParseIP("192.168.1.130"), Mask: net.CIDRMask(24, 32)})
	now = now.Add(ownAddrsFor + time.Second)
	if !o.local("192.168.1.130:1") || reads != 2 {
		t.Fatalf("an interface that came up later: reads=%d", reads)
	}
	// A list that cannot be read leaves the last one standing.
	fail = true
	now = now.Add(ownAddrsFor + time.Second)
	if !o.local("100.99.0.1:1") || o.local("100.64.0.9:1") {
		t.Fatal("the last list stands when the addresses cannot be read")
	}
}

func TestLocalGate_ARefusalIsLoggedOncePerRouteAMinute(t *testing.T) {
	s := gatedServer(t)
	var logged strings.Builder
	log.SetOutput(&logged)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })
	for i := 0; i < 5; i++ {
		gateCall(s, "PUT", "/v1/sandbox", fromHere, nil)
	}
	gateCall(s, "POST", "/v1/sessions", fromHere, withBearer("a-pane-token-that-must-not-be-logged"))
	got := logged.String()
	if strings.Count(got, `refused "PUT /v1/sandbox"`) != 1 || strings.Count(got, `refused "POST /v1/sessions"`) != 1 {
		t.Fatalf("one line per route, however often it is tried: %s", got)
	}
	if strings.Contains(got, gateToken) || strings.Contains(got, "a-pane-token") {
		t.Fatalf("no token is ever logged: %s", got)
	}
}
