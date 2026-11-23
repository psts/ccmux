package llmserve

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"reflect"
	"runtime/debug"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"ccmux.dev/ccmuxd/internal/harness"
	"ccmux.dev/ccmuxd/internal/llmproxy"
	"ccmux.dev/ccmuxd/internal/model"
	"ccmux.dev/ccmuxd/internal/store"
)

// rig is both homes of the proxy over one database: the daemon's own
// llmproxy service (it writes the settings), and the proxy service reading
// the same file, on a real listener, with the daemon's client to it.
type rig struct {
	st     *store.SQLite
	daemon *llmproxy.Service
	svc    *llmproxy.Service
	http   *httptest.Server
	client *Client
}

func newRig(t *testing.T) *rig {
	t.Helper()
	st, r, path := daemonDB(t)
	g := &rig{st: st, daemon: llmproxy.New(st), svc: NewService(r)}
	g.http = httptest.NewServer(Handler(g.svc, Config{DB: path, Unit: "unit-mark"}, nil))
	t.Cleanup(g.http.Close)
	g.client = NewClient(g.http.URL)
	return g
}

func (g *rig) configure(t *testing.T, accs []llmproxy.Account, route string) {
	t.Helper()
	if msg := g.daemon.Reject(&accs, &route); msg != "" {
		t.Fatalf("reject: %s", msg)
	}
	if err := g.daemon.Apply(&accs, &route); err != nil {
		t.Fatal(err)
	}
}

// hit is what one upstream saw of a request.
type hit struct{ path, auth, apiKey string }

// fakeUpstream answers status to everything and remembers the last request.
func fakeUpstream(t *testing.T, status int) (*httptest.Server, *atomic.Pointer[hit]) {
	t.Helper()
	var last atomic.Pointer[hit]
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		last.Store(&hit{path: r.URL.Path, auth: r.Header.Get("Authorization"), apiKey: r.Header.Get("x-api-key")})
		w.WriteHeader(status)
	}))
	t.Cleanup(up.Close)
	return up, &last
}

func post(t *testing.T, url, bearer string) *http.Response {
	t.Helper()
	req, _ := http.NewRequest("POST", url, strings.NewReader("{}"))
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	return resp
}

// The service serves the accounts the daemon configured, with the stored
// key in place of the pane's own login, and follows a change of route on
// the next request: nothing is cached, so nothing has to be pushed to it.
func TestServiceRoutesWhatTheDaemonConfigured(t *testing.T) {
	g := newRig(t)
	upA, sawA := fakeUpstream(t, 200)
	upB, sawB := fakeUpstream(t, 200)
	g.configure(t, []llmproxy.Account{
		{Name: "a", Kind: "anthropic", BaseURL: upA.URL, APIKey: "key-a"},
		{Name: "b", Kind: "openai", BaseURL: upB.URL, APIKey: "key-b"},
	}, "a")

	if resp := post(t, g.http.URL+"/llm/pane/p1/v1/messages", "the-panes-own-login"); resp.StatusCode != 200 {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	if got := sawA.Load(); got == nil || *got != (hit{path: "/v1/messages", apiKey: "key-a"}) {
		t.Fatalf("account a saw %+v, want the stored key, the pane's login dropped, the pane prefix stripped", got)
	}

	route := "b"
	if err := g.daemon.Apply(nil, &route); err != nil {
		t.Fatal(err)
	}
	post(t, g.http.URL+"/llm/pane/p1/v1/messages", "the-panes-own-login")
	if got := sawB.Load(); got == nil || got.auth != "Bearer key-b" {
		t.Fatalf("after the daemon moved the default route, account b saw %+v, want its bearer", got)
	}
}

// ROUTING EQUIVALENCE. The service resolves a pane to its harness from the
// database; the daemon did it from the manager's memory. For every kind of
// pane, both must build the same failover order, or moving the proxy out of
// the daemon changed who answers.
//
// The daemon side here is the old in-daemon wiring, fed what the manager
// holds in memory for each pane (manager.HarnessForPane; for runner panes
// see manager/runnerpanes.go). The api package runs the same comparison
// against a real manager (llmservice_test.go there).
func TestServiceOrdersPanesAsTheDaemonDid(t *testing.T) {
	g := newRig(t)
	memory := seedPanes(t, g)
	g.daemon.SetPaneHarness(HarnessRules(func(id string) string { return memory[id] }, harness.New(g.st)))

	ids := make([]string, 0, len(memory))
	for id := range memory {
		ids = append(ids, id)
	}
	want, wantFail, err := g.daemon.PaneOrders(ids)
	if err != nil {
		t.Fatal(err)
	}
	got, gotFail, err := g.svc.PaneOrders(ids)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, want) || !reflect.DeepEqual(gotFail, wantFail) {
		t.Fatalf("the service and the daemon disagree on who answers:\nservice %v (failures %v)\ndaemon  %v (failures %v)", got, gotFail, want, wantFail)
	}
	// And the agreement is about something: each tier shows up in it.
	for pane, order := range map[string][]string{
		"claude-pane":  {"sub-1", "router", "sub-2"}, // tier 2: claude's kinds, as configured
		"mine-pane":    {"sub-2", "sub-1", "router"}, // tier 2: its own account order first
		"codex-pane":   {"gpt"},
		"shell-pane":   {"router"},                   // tier 3: the default account
		"gone-harness": {"router"},                   // a harness that no longer resolves
		"overridden":   {"sub-2", "sub-1", "router"}, // tier 1: the pane's override leads
		"runner-live":  {"sub-1", "router", "sub-2"}, // a runner session runs claude
		"runner-done":  {"router"},
	} {
		if !reflect.DeepEqual(got[pane], order) {
			t.Errorf("pane %s: order %v, want %v", pane, got[pane], order)
		}
	}
}

// seedPanes fills the rig's database with accounts, a harness of its own,
// and one pane of every kind, and returns what the manager's memory would
// say each of them runs.
func seedPanes(t *testing.T, g *rig) map[string]string {
	t.Helper()
	up, _ := fakeUpstream(t, 200)
	g.configure(t, []llmproxy.Account{
		{Name: "sub-1", Kind: "claude", BaseURL: up.URL, APIKey: "t1"},
		{Name: "router", Kind: "openai", BaseURL: up.URL, APIKey: "k"},
		{Name: "sub-2", Kind: "claude", BaseURL: up.URL, APIKey: "t2"},
		{Name: "gpt", Kind: "codex", BaseURL: up.URL},
	}, "router")
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(harness.New(g.st).Apply([]harness.Harness{
		{Name: "mine", Command: "mine", AccountKinds: []string{"openai", "claude"}, AccountOrder: []string{"sub-2"}},
	}))
	must(g.st.SaveWorkspace(&model.Workspace{ID: "ws1", Name: "repo", RepoPath: "/tmp/repo", TmuxSession: "s1", Status: model.StatusLive}))
	memory := map[string]string{
		"claude-pane": "claude", "mine-pane": "mine", "codex-pane": "codex", "gone-harness": "uninstalled",
		"shell-pane": "", "overridden": "claude", "runner-live": "claude",
		"runner-done": "", "runner-orphan": "", "never-existed": "",
	}
	for id, h := range memory {
		// The tmux panes have rows; runner panes and the unknown one do not.
		if !strings.HasPrefix(id, "runner-") && id != "never-existed" {
			must(g.st.SavePane(&model.Pane{ID: id, WorkspaceID: "ws1", Harness: h}))
		}
	}
	for _, s := range []store.RunnerSession{
		{ID: "s1", Cwd: "/tmp/repo", Wanted: store.SessionRunning, PaneID: "runner-live", WorkspaceID: "ws1"},
		{ID: "s2", Cwd: "/tmp/repo", Wanted: store.SessionDone, PaneID: "runner-done", WorkspaceID: "ws1"},
		{ID: "s3", Cwd: "/tmp/gone", Wanted: store.SessionRunning, PaneID: "runner-orphan", WorkspaceID: "ws-deleted"},
	} {
		must(g.st.AddRunnerSession(s))
	}
	must(g.daemon.SetPaneRoute("overridden", "sub-2"))
	return memory
}

// limitedAccount is a rig with one account whose upstream answers 429, and
// one request already sent through the service: the service has seen the
// account hit its limit, the daemon's own copy has seen nothing.
func limitedAccount(t *testing.T) (g *rig, state func() string) {
	t.Helper()
	g = newRig(t)
	up, _ := fakeUpstream(t, http.StatusTooManyRequests)
	g.configure(t, []llmproxy.Account{{Name: "a", Kind: "anthropic", BaseURL: up.URL, APIKey: "k"}}, "a")
	post(t, g.http.URL+"/llm/pane/p1/v1/messages", "")
	return g, func() string {
		t.Helper()
		sts, err := g.daemon.Statuses()
		if err != nil || len(sts) != 1 {
			t.Fatalf("statuses = %+v, %v; want the one account", sts, err)
		}
		return sts[0].State
	}
}

// Account health is learned from traffic, and traffic goes through the
// service: the daemon has to ask it. With the service wired as the daemon's
// remote, the settings page's rows and each pane's order come from it.
func TestDaemonReadsHealthFromTheService(t *testing.T) {
	g, state := limitedAccount(t)
	if got := state(); got != "untried" {
		t.Fatalf("before the daemon asks the service its own copy read %q, want untried (it saw no traffic)", got)
	}
	g.daemon.SetRemote(g.client)
	if got := state(); got != "limited" {
		t.Fatalf("through the service the account read %q, want limited (the 429 went through it)", got)
	}
	explicit, order, err := g.daemon.PaneStatus("p1")
	if err != nil || explicit != "" || !reflect.DeepEqual(order, []string{"a"}) {
		t.Fatalf("PaneStatus through the service = %q %v %v", explicit, order, err)
	}
	orders, failures, err := g.daemon.PaneOrders([]string{"p1"})
	if err != nil || !reflect.DeepEqual(orders, map[string][]string{"p1": {"a"}}) || failures == nil {
		t.Fatalf("PaneOrders through the service = %v %v %v", orders, failures, err)
	}
}

// With the service down the three reads still answer, from the daemon's own
// copy and without the health, rather than failing the settings page.
func TestDaemonReadsStillAnswerWithTheServiceDown(t *testing.T) {
	g, state := limitedAccount(t)
	g.daemon.SetRemote(g.client)
	g.http.Close()
	if got := state(); got != "untried" {
		t.Fatalf("with the service down the account read %q, want the daemon's own untried row", got)
	}
	if _, order, err := g.daemon.PaneStatus("p1"); err != nil || !reflect.DeepEqual(order, []string{"a"}) {
		t.Fatalf("PaneStatus with the service down = %v, %v", order, err)
	}
	if orders, _, err := g.daemon.PaneOrders([]string{"p1"}); err != nil || !reflect.DeepEqual(orders["p1"], []string{"a"}) {
		t.Fatalf("PaneOrders with the service down = %v, %v", orders, err)
	}
}

// The check route answers 404 for an unknown account and 400 for one that
// cannot be checked (api.llmAccountCheck). Those are errors.Is tests on the
// daemon's side, so the two errors must still be themselves after crossing
// from the service, and say what the service said.
func TestCheckErrorsKeepTheirIdentityAcrossTheWire(t *testing.T) {
	g := newRig(t)
	up, _ := fakeUpstream(t, 200)
	g.configure(t, []llmproxy.Account{{Name: "api", Kind: "anthropic", BaseURL: up.URL, APIKey: "k"}}, "")
	g.daemon.SetRemote(g.client)

	_, err := g.daemon.CheckAccount(context.Background(), "nobody")
	if !errors.Is(err, llmproxy.ErrUnknownAccount) || errors.Is(err, llmproxy.ErrNotCheckable) {
		t.Fatalf("unknown account: %v, want ErrUnknownAccount only", err)
	}
	_, local := g.svc.CheckAccount(context.Background(), "nobody")
	if err.Error() != local.Error() {
		t.Fatalf("the message changed on the way: %q, the service said %q", err, local)
	}
	if _, err := g.daemon.CheckAccount(context.Background(), "api"); !errors.Is(err, llmproxy.ErrNotCheckable) {
		t.Fatalf("an api-key account: %v, want ErrNotCheckable", err)
	}
	// No local fallback for a check: an answer recorded in the daemon's own
	// copy would change nothing the service routes by.
	g.http.Close()
	if _, err := g.daemon.CheckAccount(context.Background(), "api"); err == nil || errors.Is(err, llmproxy.ErrNotCheckable) {
		t.Fatalf("with the service down a check answered %v, want a plain failure", err)
	}
}

// daemonFront is the daemon's old pane address: its mux pattern, handing
// what arrives to the service.
func daemonFront(t *testing.T, c *Client) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	for _, m := range []string{"GET", "POST", "PUT", "DELETE"} {
		mux.Handle(m+" /llm/pane/{pane}/{rest...}", c.Forward())
	}
	front := httptest.NewServer(mux)
	t.Cleanup(front.Close)
	return front
}

// A pane opened before the move still calls the daemon. The daemon passes
// the request on untouched, so the service's path guard sees the pane's own
// path: a dot segment that would walk a ChatGPT bearer onto another surface
// is refused there, exactly as it was when the daemon served it itself
// (llmproxy's firstSegment, and its tests for the same paths).
func TestForwardKeepsThePathGuard(t *testing.T) {
	g := newRig(t)
	up, saw := fakeUpstream(t, 200)
	g.configure(t, []llmproxy.Account{{Name: "gpt", Kind: "codex", BaseURL: up.URL}}, "gpt")
	front := daemonFront(t, g.client)

	for _, path := range []string{
		"/llm/pane/p1/responses/%2e%2e/%2e%2e/v1/messages",
		"/llm/pane/p1/x/%2e%2e/responses",
		"/llm/pane/p1/%2fresponses",
		"/llm/pane/p1/responsesXYZ",
	} {
		resp := post(t, front.URL+path, "chatgpt-oauth-token")
		if resp.StatusCode != http.StatusBadGateway {
			t.Errorf("%s through the daemon = %d, want 502", path, resp.StatusCode)
		}
		if got := saw.Load(); got != nil {
			t.Fatalf("%s reached the upstream as %s carrying %q", path, got.path, got.auth)
		}
	}
	if resp := post(t, front.URL+"/llm/pane/p1/responses?x=1", "chatgpt-oauth-token"); resp.StatusCode != 200 {
		t.Fatalf("the plain path through the daemon = %d", resp.StatusCode)
	}
	if got := saw.Load(); got == nil || got.path != "/responses" || got.auth != "Bearer chatgpt-oauth-token" {
		t.Fatalf("the upstream saw %+v, want /responses with the pane's own bearer", got)
	}
}

// A reply is a token stream. Through the daemon's forward and the service,
// the first tokens must reach the pane while the upstream is still writing.
func TestForwardStreams(t *testing.T) {
	g := newRig(t)
	release := make(chan struct{})
	var once sync.Once
	free := func() { once.Do(func() { close(release) }) }
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, "data: first\n\n")
		w.(http.Flusher).Flush()
		<-release
		io.WriteString(w, "data: last\n\n")
	}))
	defer up.Close()
	defer free()
	g.configure(t, []llmproxy.Account{{Name: "a", Kind: "anthropic", BaseURL: up.URL, APIKey: "k"}}, "a")
	front := daemonFront(t, g.client)

	resp := post(t, front.URL+"/llm/pane/p1/v1/messages", "")
	buf := make([]byte, len("data: first\n\n"))
	got := make(chan error, 1)
	go func() {
		_, err := io.ReadFull(resp.Body, buf)
		got <- err
	}()
	select {
	case err := <-got:
		if err != nil || string(buf) != "data: first\n\n" {
			t.Fatalf("first chunk = %q, %v", buf, err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the first chunk did not arrive while the upstream was still writing: something buffers the stream")
	}
	free()
	rest, _ := io.ReadAll(resp.Body)
	if string(rest) != "data: last\n\n" {
		t.Fatalf("rest of the stream = %q", rest)
	}
}

// With the service down, a pane on the daemon's old address is told so.
func TestForwardSaysWhenTheServiceIsDown(t *testing.T) {
	g := newRig(t)
	front := daemonFront(t, g.client)
	g.http.Close()
	resp := post(t, front.URL+"/llm/pane/p1/v1/messages", "")
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusBadGateway || !strings.Contains(string(body), "proxy service is not answering") {
		t.Fatalf("answer = %d %q", resp.StatusCode, body)
	}
}

// Keyed accounts inject stored credentials: nothing off this machine may
// use them, on the service's own port as on the daemon's.
func TestServiceIsLoopbackOnly(t *testing.T) {
	g := newRig(t)
	h := Handler(g.svc, Config{DB: "db"}, nil)
	for _, path := range []string{"/llm/pane/p1/v1/messages", "/health", "/ctl/statuses"} {
		req := httptest.NewRequest("GET", path, nil)
		req.RemoteAddr = "100.64.1.2:9999" // a tailnet caller
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusForbidden {
			t.Errorf("%s from off the machine = %d, want 403", path, rec.Code)
		}
	}
	if err := Serve(context.Background(), Config{DB: "x", Addr: "0.0.0.0:0"}); err == nil || !strings.Contains(err.Error(), "loopback") {
		t.Fatalf("Serve on every interface = %v, want a refusal", err)
	}
}

// GET /health is how the daemon tells its own service from somebody else's,
// and current code from old.
func TestHealthNamesTheCodeAndTheDatabase(t *testing.T) {
	g := newRig(t)
	h, err := g.client.Health(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !h.OK || h.CodeHash != CodeHash() || h.CodeHash == "" || !strings.HasSuffix(h.DB, "ccmuxd.db") || h.Unit != "unit-mark" {
		t.Fatalf("health = %+v", h)
	}
	resp, err := http.Get(g.http.URL + "/health")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var raw map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&raw); err != nil {
		t.Fatal(err)
	}
	// build-daemon.sh greps this key out of the answer.
	if _, ok := raw["codeHash"].(string); !ok {
		t.Fatalf("health JSON has no codeHash string: %v", raw)
	}
}

// A stop lets a request in flight finish before the process goes.
func TestStopLetsARequestInFlightFinish(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	entered, release := make(chan struct{}), make(chan struct{})
	h := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		close(entered)
		<-release
		io.WriteString(w, "whole")
	})
	ctx, stop := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- serveOn(ctx, ln, h, 5*time.Second) }()

	body := make(chan string, 1)
	go func() {
		resp, err := http.Get("http://" + ln.Addr().String() + "/")
		if err != nil {
			body <- "error: " + err.Error()
			return
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		body <- string(b)
	}()
	<-entered
	stop()
	select {
	case err := <-done:
		t.Fatalf("the service stopped (%v) with a request still in flight", err)
	case <-time.After(200 * time.Millisecond):
	}
	close(release)
	if got := <-body; got != "whole" {
		t.Fatalf("the request in flight got %q, want its whole answer", got)
	}
	if err := <-done; err != nil {
		t.Fatalf("serveOn = %v", err)
	}
}

func TestUnstampedHash(t *testing.T) {
	info := func(kv ...string) *debug.BuildInfo {
		b := &debug.BuildInfo{}
		for i := 0; i < len(kv); i += 2 {
			b.Settings = append(b.Settings, debug.BuildSetting{Key: kv[i], Value: kv[i+1]})
		}
		return b
	}
	for _, c := range []struct {
		name string
		info *debug.BuildInfo
		ok   bool
		want string
	}{
		{"no build info", nil, false, "unstamped"},
		{"no commit (a test binary)", info("GOOS", "linux"), true, "unstamped"},
		{"a clean commit", info("vcs.revision", "abc123", "vcs.modified", "false"), true, "commit:abc123"},
		{"a dirty tree", info("vcs.revision", "abc123", "vcs.modified", "true"), true, "commit:abc123+dirty"},
	} {
		if got := unstampedHash(c.info, c.ok); got != c.want {
			t.Errorf("%s: %q, want %q", c.name, got, c.want)
		}
	}
}
