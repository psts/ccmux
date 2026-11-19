package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"ccmux.dev/ccmuxd/internal/llmserve"
)

// linuxHost is a Linux machine with systemd and an empty unit folder.
func linuxHost(t *testing.T) proxyHost {
	t.Helper()
	return proxyHost{goos: "linux", systemctl: true, unitDir: t.TempDir(), bin: "/opt/ccmux/ccmuxd", path: "/usr/bin:/bin"}
}

// Where the proxy runs is decided once, at start. Everything but the
// installed Linux daemon keeps it in the daemon's own process.
func TestPlanLLMProxyDecidesWhereItRuns(t *testing.T) {
	db := "/home/u/.config/ccmuxd/ccmuxd.db"
	for _, c := range []struct {
		name     string
		unitFlag string
		host     func(h proxyHost) proxyHost
		wantUnit string
		wantWhy  string // a piece of the reason, when it runs in the daemon
	}{
		{"the installed daemon", "", nil, "ccmux-llm-proxy.service", ""},
		{"macOS", "", func(h proxyHost) proxyHost { h.goos = "darwin"; return h }, "", "needs systemd"},
		{"macOS, even when a unit is named", "mine", func(h proxyHost) proxyHost { h.goos = "darwin"; return h }, "", "needs systemd"},
		{"no systemd", "", func(h proxyHost) proxyHost { h.systemctl = false; return h }, "", "no systemctl"},
		{"switched off", llmProxyInProcess, nil, "", "-llm-proxy-unit none"},
		{"a scratch daemon", "", func(h proxyHost) proxyHost { h.scratch = "/run/user/1000/t1"; return h }, "", "scratch daemon"},
		{"a scratch daemon with its own unit", "ccmux-llm-proxy-t1", func(h proxyHost) proxyHost { h.scratch = "/run/user/1000/t1"; return h }, "ccmux-llm-proxy-t1.service", ""},
	} {
		host := linuxHost(t)
		if c.host != nil {
			host = c.host(host)
		}
		p := planLLMProxy(c.unitFlag, "", "127.0.0.1:7900", db, host)
		if p.unit != c.wantUnit {
			t.Errorf("%s: unit %q, want %q (why: %s)", c.name, p.unit, c.wantUnit, p.why)
		}
		if c.wantUnit == "" && (p.URL() != "" || !strings.Contains(p.why, c.wantWhy)) {
			t.Errorf("%s: url %q, why %q; want no url and a reason naming %q", c.name, p.URL(), p.why, c.wantWhy)
		}
		if c.wantUnit != "" && p.URL() != "http://127.0.0.1:7901" {
			t.Errorf("%s: url %q, want the port after the daemon's", c.name, p.URL())
		}
	}
}

// THE GUARD. A second daemon on the host (a test's, one from another
// checkout, with its own database) finds the live daemon's unit on disk. It
// must leave it exactly as it is and run its proxy in its own process:
// rewriting it would point the live service at a scratch database and
// restart it under every live session.
func TestASecondDaemonNeverTouchesTheLiveUnit(t *testing.T) {
	host := linuxHost(t)
	live := planLLMProxy("", "", "127.0.0.1:7900", "/home/u/.config/ccmuxd/ccmuxd.db", host)
	if _, err := live.writeUnitWith(&fakeUnits{}); err != nil {
		t.Fatal(err)
	}
	unitPath := filepath.Join(host.unitDir, "ccmux-llm-proxy.service")
	before, err := os.ReadFile(unitPath)
	if err != nil {
		t.Fatal(err)
	}

	second := planLLMProxy("", "", "127.0.0.1:7911", "/tmp/scratch/ccmuxd.db", host)
	if second.unit != "" || second.URL() != "" {
		t.Fatalf("the second daemon took the unit %q (url %q)", second.unit, second.URL())
	}
	if !strings.Contains(second.why, "another daemon") || !strings.Contains(second.why, "/home/u/.config/ccmuxd/ccmuxd.db") {
		t.Fatalf("why = %q, want it to say whose unit it is", second.why)
	}
	if after, _ := os.ReadFile(unitPath); string(after) != string(before) {
		t.Fatal("the live unit file changed")
	}

	// The live daemon, starting again, still owns it.
	if again := planLLMProxy("", "", "127.0.0.1:7900", "/home/u/.config/ccmuxd/ccmuxd.db", host); again.unit == "" {
		t.Fatalf("the owner was refused its own unit: %s", again.why)
	}
	// A unit file with no owner line is not this daemon's to take either.
	if err := os.WriteFile(unitPath, []byte("[Service]\nExecStart=/bin/true\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if p := planLLMProxy("", "", "127.0.0.1:7900", "/home/u/.config/ccmuxd/ccmuxd.db", host); p.unit != "" {
		t.Fatal("a unit file somebody else wrote was taken over")
	}
}

func TestProxyAddr(t *testing.T) {
	for _, c := range []struct{ flag, daemon, want string }{
		{"", "127.0.0.1:7900", "127.0.0.1:7901"},
		{"", ":7890", "127.0.0.1:7891"},
		{"", "0.0.0.0:7911", "127.0.0.1:7912"},
		{"127.0.0.1:9000", "127.0.0.1:7900", "127.0.0.1:9000"},
		{"", "no-port", ""},
		{"", "127.0.0.1:65535", ""},
	} {
		if got := proxyAddr(c.flag, c.daemon); got != c.want {
			t.Errorf("proxyAddr(%q, %q) = %q, want %q", c.flag, c.daemon, got, c.want)
		}
	}
	p := planLLMProxy("", "", "no-port", "/x/ccmuxd.db", linuxHost(t))
	if p.unit != "" || !strings.Contains(p.why, "no address") {
		t.Fatalf("a daemon address with no port: unit %q, why %q", p.unit, p.why)
	}
}

func TestProxyUnitText(t *testing.T) {
	host := linuxHost(t)
	host.bin = "/opt/my ccmux/ccmuxd"
	host.path = "/home/u/100%/bin:/usr/bin"
	p := planLLMProxy("", "", "127.0.0.1:7900", "/data/ccmuxd.db", host)
	text := p.unitText()
	for _, want := range []string{
		"# ccmux-llm-proxy-db=/data/ccmuxd.db\n", // the owner line, first
		`ExecStart="/opt/my ccmux/ccmuxd" llm-proxy -db /data/ccmuxd.db -addr 127.0.0.1:7901 -unit ` + p.unitMark() + "\n",
		"Environment=PATH=/home/u/100%%/bin:/usr/bin\n", // the daemon's own PATH, % escaped
		"Restart=always\n",
		"StartLimitIntervalSec=0\n", // it keeps coming back: every pane needs it
		"KillMode=control-group\n",
		"WantedBy=default.target\n",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("unit file lacks %q:\n%s", want, text)
		}
	}
	if !strings.HasPrefix(text, proxyDBMark) {
		t.Error("the owner line is not the first line")
	}
	if got := unitServesOther(writeTemp(t, text), "/data/ccmuxd.db"); got != "" {
		t.Errorf("the unit this daemon renders reads as another's (%q)", got)
	}
	// The mark names the unit file's text: any change to the unit moves it,
	// and the same unit gives the same mark.
	moved := planLLMProxy("", "127.0.0.1:9999", "127.0.0.1:7900", "/data/ccmuxd.db", host)
	if p.unitMark() == "" || moved.unitMark() == p.unitMark() {
		t.Errorf("marks %q and %q: a unit on another address must have another mark", p.unitMark(), moved.unitMark())
	}
	if again := planLLMProxy("", "", "127.0.0.1:7900", "/data/ccmuxd.db", host); again.unitMark() != p.unitMark() {
		t.Error("the same unit gave two marks")
	}
}

func writeTemp(t *testing.T, text string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "u.service")
	if err := os.WriteFile(path, []byte(text), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

// fakeUnits records what the daemon asked of systemd. onRestart and onStart
// stand in for what the real thing would do to the service.
type fakeUnits struct {
	mu        sync.Mutex
	calls     []string
	onRestart func()
	onStart   func()
	// reloadErr fails the next daemon-reload, once.
	reloadErr error
}

func (f *fakeUnits) note(call string, then func()) error {
	f.mu.Lock()
	f.calls = append(f.calls, call)
	f.mu.Unlock()
	if then != nil {
		then()
	}
	return nil
}

func (f *fakeUnits) Start(u string) error   { return f.note("start "+u, f.onStart) }
func (f *fakeUnits) Restart(u string) error { return f.note("restart "+u, f.onRestart) }
func (f *fakeUnits) Enable(u string) error  { return f.note("enable "+u, nil) }
func (f *fakeUnits) Reload() error {
	_ = f.note("daemon-reload", nil)
	err := f.reloadErr
	f.reloadErr = nil
	return err
}

// got is what was asked since the last look.
func (f *fakeUnits) got() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	calls := f.calls
	f.calls = nil
	return calls
}

// writeUnitWith is writeUnit against the given systemd.
func (p *llmProxy) writeUnitWith(u proxyUnits) (bool, error) {
	p.units = u
	return p.writeUnit()
}

// fakeService answers GET /health as a running proxy service would. A nil
// answer is a service that is down.
type fakeService struct {
	mu     sync.Mutex
	answer *llmserve.Health
	srv    *httptest.Server
}

func newFakeService(t *testing.T) *fakeService {
	t.Helper()
	f := &fakeService{}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		f.mu.Lock()
		h := f.answer
		f.mu.Unlock()
		if h == nil {
			http.Error(w, "down", http.StatusServiceUnavailable)
			return
		}
		_ = json.NewEncoder(w).Encode(h)
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeService) set(h *llmserve.Health) {
	f.mu.Lock()
	f.answer = h
	f.mu.Unlock()
}

// proxyOn is the installed daemon's plan, with its service at the fake's
// address and systemd faked.
func proxyOn(t *testing.T, svc *fakeService, units *fakeUnits) *llmProxy {
	t.Helper()
	addr := strings.TrimPrefix(svc.srv.URL, "http://")
	p := planLLMProxy("", addr, "127.0.0.1:7900", "/data/ccmuxd.db", linuxHost(t))
	if p.unit == "" {
		t.Fatalf("no service planned: %s", p.why)
	}
	p.units, p.wait = units, 2*time.Second
	return p
}

// What a starting daemon does to the service, case by case. The point of
// the whole feature is the third row: same unit, same code, nothing touched.
func TestBringUpRestartsOnlyOnAChange(t *testing.T) {
	const unit = "ccmux-llm-proxy.service"
	current := func(p *llmProxy) *llmserve.Health { h := p.wantHealth(); return &h }
	oldCode := func(p *llmProxy) *llmserve.Health { h := p.wantHealth(); h.CodeHash = "an-older-build"; return &h }
	oldUnit := func(p *llmProxy) *llmserve.Health { h := p.wantHealth(); h.Unit = "another-unit"; return &h }
	down := func(*llmProxy) *llmserve.Health { return nil }
	touched := func(verb string) []string { return []string{"daemon-reload", "enable " + unit, verb + " " + unit} }
	for _, c := range []struct {
		name       string
		unitOnDisk bool // the unit file is already there and current
		running    func(*llmProxy) *llmserve.Health
		wantCalls  []string
		wantAction string
	}{
		{"first start ever", false, down, touched("restart"), "restarted: its unit file is new"},
		{"the unit file changed under a running service", false, oldUnit, touched("restart"), "restarted: its unit file is new or changed"},
		{"a deploy that did not touch the proxy", true, current, []string{"enable " + unit}, "left running"},
		{"a deploy that changed the proxy", true, oldCode, touched("restart"), "restarted: its code changed (an-older-build to"},
		{"the service is down", true, down, touched("start"), "started: it was not running"},
		// The file was put back (deleted by hand, say) under a process that
		// IS the current unit: systemd is told, the process is left alone.
		{"the unit file was missing under a current service", false, current, []string{"daemon-reload", "enable " + unit}, "left running"},
	} {
		svc := newFakeService(t)
		units := &fakeUnits{}
		p := proxyOn(t, svc, units)
		svc.set(c.running(p))
		up := func() { svc.set(current(p)) } // systemd brings this daemon's service up
		units.onRestart, units.onStart = up, up
		if c.unitOnDisk {
			if _, err := p.writeUnitWith(&fakeUnits{}); err != nil {
				t.Fatal(err)
			}
			p.units = units
		}
		action, err := p.bringUp(context.Background())
		if err != nil {
			t.Errorf("%s: %v", c.name, err)
		}
		if got := units.got(); !reflect.DeepEqual(got, c.wantCalls) {
			t.Errorf("%s: asked systemd for %v, want %v", c.name, got, c.wantCalls)
		}
		if !strings.Contains(action, c.wantAction) {
			t.Errorf("%s: action %q, want it to say %q", c.name, action, c.wantAction)
		}
	}
}

// startOnce runs one start of the daemon against the service and checks
// what it says it did and what it asked of systemd.
func startOnce(t *testing.T, p *llmProxy, units *fakeUnits, wantAction string, wantCalls ...string) error {
	t.Helper()
	action, err := p.bringUp(context.Background())
	if !strings.Contains(action, wantAction) {
		t.Fatalf("action %q (err %v), want it to say %q", action, err, wantAction)
	}
	if got := units.got(); !reflect.DeepEqual(got, wantCalls) {
		t.Fatalf("asked systemd for %v, want %v", got, wantCalls)
	}
	return err
}

// systemd fails to reload its unit files on the start that wrote a changed
// unit. The file is then current on disk while the running process is not,
// and the NEXT start must still restart the service: it goes by what the
// process says it was started from, not by the file. The start after that,
// with everything current, touches nothing.
func TestAFailedReloadIsMadeUpForOnTheNextStart(t *testing.T) {
	const unit = "ccmux-llm-proxy.service"
	svc := newFakeService(t)
	units := &fakeUnits{reloadErr: errors.New("Failed to reload daemon: Connection timed out")}
	p := proxyOn(t, svc, units)
	stale := p.wantHealth()
	stale.Unit = "the-unit-before-the-change"
	svc.set(&stale)
	units.onRestart = func() { h := p.wantHealth(); svc.set(&h) }

	// Nothing may be started from units systemd has not read, and the
	// action must not blame the file: it was written.
	if err := startOnce(t, p, units, "NOT restarted (its unit file is new or changed): systemd did not reload", "daemon-reload"); err == nil {
		t.Fatal("the start whose reload failed reported no error")
	}
	if _, err := os.Stat(filepath.Join(p.host.unitDir, unit)); err != nil {
		t.Fatalf("the unit file is not on disk: %v", err)
	}
	if err := startOnce(t, p, units, "restarted: its unit file is new or changed", "daemon-reload", "enable "+unit, "restart "+unit); err != nil {
		t.Fatalf("the next start, which owes the restart: %v", err)
	}
	if err := startOnce(t, p, units, "left running", "enable "+unit); err != nil {
		t.Fatalf("a start with everything current: %v", err)
	}
}

// Another daemon's proxy on the port is left alone and reported, not
// restarted: restarting this daemon's unit would not move it anyway.
func TestBringUpLeavesAnotherDaemonsServiceAlone(t *testing.T) {
	svc := newFakeService(t)
	svc.set(&llmserve.Health{OK: true, CodeHash: llmserve.CodeHash(), DB: "/somebody/elses.db"})
	units := &fakeUnits{}
	p := proxyOn(t, svc, units)
	if _, err := p.writeUnitWith(&fakeUnits{}); err != nil {
		t.Fatal(err)
	}
	p.units = units
	action, err := p.bringUp(context.Background())
	if err == nil || !strings.Contains(err.Error(), "/somebody/elses.db") || !strings.Contains(action, "left alone") {
		t.Fatalf("bringUp = %q, %v; want it left alone, naming the other database", action, err)
	}
	if got := units.got(); len(got) != 0 {
		t.Fatalf("asked systemd for %v, want nothing", got)
	}
}

// A service that never comes up is an error the health answer carries, so
// the deploy script can fail on it.
func TestHealthReportsAServiceThatDidNotComeUp(t *testing.T) {
	svc := newFakeService(t) // down, and the fake systemd never brings it up
	p := proxyOn(t, svc, &fakeUnits{})
	p.wait = 300 * time.Millisecond
	p.ensure(context.Background())
	h := p.health()
	if h["mode"] != "service" || h["serving"] != false || h["error"] == nil || h["error"] == "" {
		t.Fatalf("health = %v, want a service that is not serving, with the reason", h)
	}
	if h["url"] != svc.srv.URL || h["wantHash"] != llmserve.CodeHash() {
		t.Fatalf("health = %v", h)
	}

	up := p.wantHealth()
	svc.set(&up)
	p.probe = proxyProbe{} // past the two seconds an answer is kept
	p.ensure(context.Background())
	h = p.health()
	if h["serving"] != true || h["error"] != nil || h["codeHash"] != llmserve.CodeHash() || !strings.Contains(h["action"].(string), "left running") {
		t.Fatalf("health once it is up = %v", h)
	}
}

func TestHealthInProcessSaysWhy(t *testing.T) {
	p := planLLMProxy(llmProxyInProcess, "", "127.0.0.1:7900", "/data/ccmuxd.db", linuxHost(t))
	h := p.health()
	if h["mode"] != "in-process" || h["why"] == "" {
		t.Fatalf("health = %v", h)
	}
}
