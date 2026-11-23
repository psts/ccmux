package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"log"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"ccmux.dev/ccmuxd/internal/api"
	"ccmux.dev/ccmuxd/internal/llmproxy"
	"ccmux.dev/ccmuxd/internal/llmserve"
	"ccmux.dev/ccmuxd/internal/manager"
	"ccmux.dev/ccmuxd/internal/runner"
)

// The LLM proxy's own service. On Linux the proxy does not run in this
// process: it is `ccmuxd llm-proxy` (internal/llmserve) under a systemd user
// unit, so that restarting the daemon, which every deploy does, cannot cut a
// reply a session is streaming. This file is the daemon's side of that: the
// unit file, who owns it, and bringing the service up when the daemon starts.
//
// The service is restarted in exactly two cases: its unit file changed, or
// its code did (llmserve.CodeHash differs from this binary's). Otherwise a
// starting daemon leaves it alone.

// llmProxyUnitDefault is the installed daemon's unit. A second daemon on the
// host must name its own (-llm-proxy-unit) or run the proxy in its process.
const llmProxyUnitDefault = "ccmux-llm-proxy"

// llmProxyInProcess is the -llm-proxy-unit value that keeps the proxy in the
// daemon, as it was before it had a service.
const llmProxyInProcess = "none"

// proxyUnits is what the proxy's service asks of systemd.
type proxyUnits interface {
	Start(unit string) error
	Restart(unit string) error
	Reload() error
	Enable(unit string) error
}

// proxySystemctl is proxyUnits through `systemctl --user`.
type proxySystemctl struct{ runner.Systemctl }

func (proxySystemctl) Enable(unit string) error {
	out, err := exec.Command("systemctl", "--user", "enable", unit).CombinedOutput()
	if err != nil {
		return fmt.Errorf("systemctl enable %s: %v: %s", unit, err, strings.TrimSpace(string(out)))
	}
	return nil
}

// proxyHost is what the decision below reads from the machine, so a test can
// say it.
type proxyHost struct {
	goos      string
	systemctl bool   // systemctl is on the PATH
	scratch   string // CCMUXD_ROOT
	unitDir   string // the user's systemd folder
	bin       string // this binary
	path      string // this process's PATH
}

func thisProxyHost(goos string) proxyHost {
	_, err := exec.LookPath("systemctl")
	return proxyHost{
		goos: goos, systemctl: err == nil, scratch: scratchRoot(),
		unitDir: systemdUserDir(), bin: selfExecutable(), path: os.Getenv("PATH"),
	}
}

// llmProxy is where the LLM proxy runs for this daemon: in its own service
// (unit set), or in this process (unit empty, why says why).
type llmProxy struct {
	unit string // "<name>.service"; "" = in this process
	why  string
	addr string
	db   string
	host proxyHost

	units  proxyUnits
	client *llmserve.Client
	// wait is how long a start of the daemon waits for the service to
	// answer after starting or restarting it: past the old one's stop
	// timeout (20s, unitText) and the new one's start.
	wait time.Duration

	mu     sync.Mutex
	action string // what the last start of this daemon did to the service
	failed string // why the service is not up, "" when it is
	probe  proxyProbe
}

// planLLMProxy decides where the proxy runs. It only READS: nothing is
// written or started until wire, and never for a unit this daemon does not
// own.
//
// In this process when: the host has no systemd (macOS, a container); the
// flag says so; this is a scratch daemon (CCMUXD_ROOT) that named no unit of
// its own; or the unit file on disk serves ANOTHER database. That last one
// is the guard that matters: a second daemon on the host (a test's, one
// built from another checkout) would otherwise rewrite the live service to
// read its own scratch database and restart it under every live session.
func planLLMProxy(unitFlag, addrFlag, daemonAddr, db string, host proxyHost) *llmProxy {
	p := &llmProxy{db: db, host: host, units: proxySystemctl{}, wait: 30 * time.Second}
	name, why := proxyUnitName(unitFlag, host)
	if name == "" {
		p.why = why
		return p
	}
	abs, err := filepath.Abs(db)
	if err != nil {
		p.why = "the database path " + db + " cannot be made absolute: " + err.Error()
		return p
	}
	p.db = abs
	if p.addr = proxyAddr(addrFlag, daemonAddr); p.addr == "" {
		p.why = "no address for it: -addr " + daemonAddr + " has no port to count from, and -llm-proxy-addr is not set"
		return p
	}
	unit := name + ".service"
	if other := unitServesOther(filepath.Join(host.unitDir, unit), p.db); other != "" {
		p.why = "the unit " + unit + " belongs to another daemon (it serves " + other + "): pass -llm-proxy-unit to give this one its own, or if that unit is a leftover, remove " + filepath.Join(host.unitDir, unit) + " and restart ccmuxd"
		return p
	}
	p.unit = unit
	p.client = llmserve.NewClient("http://" + p.addr)
	return p
}

// proxyUnitName is the unit's name without ".service", or "" and why not.
func proxyUnitName(flagValue string, host proxyHost) (name, why string) {
	switch {
	case flagValue == llmProxyInProcess:
		return "", "-llm-proxy-unit " + llmProxyInProcess
	case host.goos != "linux":
		return "", "the proxy's service needs systemd, and this is " + host.goos
	case !host.systemctl:
		return "", "no systemctl on this host"
	case flagValue != "":
		return flagValue, ""
	case host.scratch != "":
		return "", "a scratch daemon (CCMUXD_ROOT) that named no -llm-proxy-unit of its own"
	}
	return llmProxyUnitDefault, ""
}

// proxyAddr is where the service listens: the flag, else the port after the
// daemon's on loopback (7901 beside a daemon on 7900), so two daemons on one
// host do not meet on one port by default.
func proxyAddr(flagValue, daemonAddr string) string {
	if flagValue != "" {
		return flagValue
	}
	_, port, err := net.SplitHostPort(daemonAddr)
	if err != nil {
		return ""
	}
	n, err := strconv.Atoi(port)
	if err != nil || n <= 0 || n >= 65535 {
		return ""
	}
	return net.JoinHostPort("127.0.0.1", strconv.Itoa(n+1))
}

// URL is the base a pane's ANTHROPIC_BASE_URL is built on, "" when the
// proxy runs in this daemon (the manager then uses the daemon's own).
func (p *llmProxy) URL() string {
	if p.unit == "" {
		return ""
	}
	return p.client.URL()
}

// proxyDBMark is the line of the unit file that says whose it is. It is a
// comment to systemd and the ownership record to ccmuxd: KEEP IT, in this
// exact form, whatever else about the unit changes. A daemon that does not
// find its own database on this line treats the unit as another daemon's
// and will not touch it.
const proxyDBMark = "# ccmux-llm-proxy-db="

// unitServesOther reads the unit file at path and returns the database it
// serves when that is not db; "" when the file is missing or serves db. A
// file with no mark at all is not this daemon's to take either, and reads
// as "(no database named)".
func unitServesOther(path, db string) string {
	raw, err := os.ReadFile(path)
	if err != nil {
		return "" // missing: free to create. Unreadable: the write will say so.
	}
	for _, line := range strings.Split(string(raw), "\n") {
		if served, ok := strings.CutPrefix(line, proxyDBMark); ok {
			if served == db {
				return ""
			}
			return served
		}
	}
	return "(no database named)"
}

// unitText is the service's unit file.
//
// PATH is this daemon's own. The proxy finds which harnesses are installed
// the way the daemon does (harness detection walks PATH), and a harness one
// of them sees and the other does not would route that harness's panes
// differently in the two.
//
// Restart=always, with no start limit: every pane on the host needs this
// service to reach its model, so it comes back however it went (a crash, a
// stray kill, a port that was busy for a minute), every two seconds for as
// long as it takes. `systemctl stop` still stops it. KillMode=mixed: the
// stop signal goes to the service alone, which drains (llmserve's, 10s) and
// then stops its Meridian sidecars itself (up to 5s for their children);
// control-group would signal the sidecars at once and cut the replies the
// drain waits for. Whatever is left when the service has exited, or at
// TimeoutStopSec, is killed with the whole group, so none outlives it.
func (p *llmProxy) unitText() string { return p.unitTextWith(p.unitMark()) }

// unitMark names the unit file this daemon would write: a short hash of its
// text. It rides in ExecStart (-unit) and the service repeats it on GET
// /health, which is how a starting daemon knows whether the process that is
// RUNNING was started from the current unit file. The file on disk cannot
// say that: it says what systemd will start next, and it can be current
// while the process is not (a unit written on an earlier start that systemd
// never got to load).
func (p *llmProxy) unitMark() string {
	sum := sha256.Sum256([]byte(p.unitTextWith("")))
	return hex.EncodeToString(sum[:6])
}

func (p *llmProxy) unitTextWith(mark string) string {
	path := p.host.path
	if path == "" {
		path = "/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"
	}
	return fmt.Sprintf(`%s%s
[Unit]
Description=ccmux LLM proxy (ccmuxd llm-proxy)
StartLimitIntervalSec=0

[Service]
Type=simple
ExecStart=%s llm-proxy -db %s -addr %s -unit %s
Environment=%s
Restart=always
RestartSec=2
KillMode=mixed
TimeoutStopSec=20
StandardInput=null

[Install]
WantedBy=default.target
`, proxyDBMark, p.db, unitWord(p.host.bin), unitWord(p.db), p.addr, mark, unitWord("PATH="+path))
}

// unitWord quotes one word of a unit file line: "%" is systemd's specifier
// sign, and a word with a space or a quote in it needs quoting.
func unitWord(s string) string {
	s = strings.ReplaceAll(s, "%", "%%")
	if !strings.ContainsAny(s, " \t\"'\\") {
		return s
	}
	s = strings.ReplaceAll(s, `\`, `\\`)
	return `"` + strings.ReplaceAll(s, `"`, `\"`) + `"`
}

// wire connects the daemon to wherever the proxy runs, and when that is its
// own service, starts bringing it up. The health part is wired either way:
// GET /v1/health says which of the two this daemon is doing.
func (p *llmProxy) wire(ctx context.Context, svc *llmproxy.Service, apiSrv *api.Server) {
	apiSrv.SetLLMProxyHealth(p.health)
	if p.unit == "" {
		log.Printf("llm proxy: running in this daemon (%s); a daemon restart cuts replies being streamed", p.why)
		return
	}
	// Account health lives where the traffic is, so the reads built on it
	// are the service's to answer; and the panes that still hold this
	// daemon's address are passed on to it.
	svc.SetRemote(p.client)
	apiSrv.SetLLMForward(p.client.Forward())
	go p.ensure(ctx)
}

// ensure brings the service up for this start of the daemon and records
// what it did. Off the start path: a restart waits for the old service's
// drain, and the daemon must not hold its own listener back for that.
func (p *llmProxy) ensure(ctx context.Context) {
	action, err := p.bringUp(ctx)
	p.mu.Lock()
	p.action = action
	p.failed = ""
	if err != nil {
		p.failed = err.Error()
	}
	p.mu.Unlock()
	if ctx.Err() != nil {
		return // the daemon is stopping; the service is not its to judge now
	}
	if err != nil {
		log.Printf("llm proxy: service %s needs a look (%s): %v; while it is not up, panes cannot reach their models (journalctl --user -u %s)", p.unit, action, err, p.unit)
		return
	}
	log.Printf("llm proxy: service %s at %s: %s", p.unit, p.client.URL(), action)
}

func (p *llmProxy) bringUp(ctx context.Context) (string, error) {
	wrote, err := p.writeUnit()
	if err != nil {
		return "its unit file could not be written", err
	}
	step, err := p.decide(ctx, wrote)
	if err != nil {
		return step.done(), err
	}
	// systemd reads the unit files again before anything is started from
	// one. Before EVERY start and restart, not only after a write: a reload
	// that failed on an earlier start of the daemon is made up for by the
	// next one, instead of the old unit being started again for good.
	if wrote || step.do != nil {
		if err := p.units.Reload(); err != nil {
			return step.notDone("systemd did not reload its unit files"), err
		}
	}
	if err := p.units.Enable(p.unit); err != nil {
		// It still runs now; it only will not start by itself at boot, where
		// the daemon's own start brings it up a moment later anyway.
		log.Printf("llm proxy: %v", err)
	}
	if step.do == nil {
		return step.done(), nil
	}
	if err := step.do(p.unit); err != nil {
		return step.notDone("systemd refused"), err
	}
	return step.done(), p.waitServing(ctx, p.wait)
}

// writeUnit writes the unit file when it is missing or differs, and reports
// whether it did. Only the file: telling systemd is bringUp's, which does it
// before anything is started from it.
func (p *llmProxy) writeUnit() (wrote bool, err error) {
	path := filepath.Join(p.host.unitDir, p.unit)
	want := []byte(p.unitText())
	if have, err := os.ReadFile(path); err == nil && bytes.Equal(have, want) {
		return false, nil
	}
	if err := os.MkdirAll(p.host.unitDir, 0o755); err != nil {
		return false, err
	}
	return true, os.WriteFile(path, want, 0o644)
}

// proxyStep is what a starting daemon does to the service: verb and why for
// the log and the health answer, do for systemd (nil leaves it alone).
type proxyStep struct {
	verb, why string
	do        func(unit string) error
}

func (s proxyStep) done() string { return s.verb + ": " + s.why }

func (s proxyStep) notDone(because string) string {
	if s.do == nil {
		return s.done() + " (" + because + ")"
	}
	return "NOT " + s.verb + " (" + s.why + "): " + because
}

// decide is the whole rule for touching a running service. It is restarted
// when the process that answers was started from another unit file than the
// current one, or is built from other proxy code; and left alone otherwise.
func (p *llmProxy) decide(ctx context.Context, wroteUnit bool) (proxyStep, error) {
	h, err := p.client.Health(ctx)
	switch {
	case err != nil && wroteUnit:
		// Nothing answers where the new unit says to listen: a first start,
		// or the address moved and the old process is still on the old one.
		return proxyStep{"restarted", "its unit file is new or changed", p.units.Restart}, nil
	case err != nil:
		// Not answering. Start is a no-op on a unit that is already coming
		// up, so one failed probe of a busy service never cuts it short.
		return proxyStep{"started", "it was not running", p.units.Start}, nil
	case h.DB != p.db:
		return proxyStep{verb: "left alone", why: "another daemon's proxy holds its port"},
			fmt.Errorf("what answers on %s reads %s, not this daemon's %s (set -llm-proxy-addr)", p.addr, h.DB, p.db)
	case h.Unit != p.unitMark():
		return proxyStep{"restarted", "its unit file is new or changed", p.units.Restart}, nil
	case h.CodeHash != llmserve.CodeHash():
		return proxyStep{"restarted", "its code changed (" + h.CodeHash + " to " + llmserve.CodeHash() + ")", p.units.Restart}, nil
	}
	return proxyStep{verb: "left running", why: "its code has not changed"}, nil
}

// waitServing waits for the service to answer as this daemon's own: its
// database, the current unit file, this binary's proxy code.
func (p *llmProxy) waitServing(ctx context.Context, limit time.Duration) error {
	deadline := time.Now().Add(limit)
	for {
		h, err := p.client.Health(ctx)
		if err == nil && h == p.wantHealth() {
			return nil
		}
		if time.Now().After(deadline) {
			if err == nil {
				err = fmt.Errorf("it answers as %+v; this daemon expects %+v", h, p.wantHealth())
			}
			return fmt.Errorf("not serving after %s: %w", limit, err)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(250 * time.Millisecond):
		}
	}
}

// wantHealth is the health answer of this daemon's own service, as it
// should be running now.
func (p *llmProxy) wantHealth() llmserve.Health {
	return llmserve.Health{OK: true, CodeHash: llmserve.CodeHash(), DB: p.db, Unit: p.unitMark()}
}

// proxyProbe is the last health answer, kept a moment: GET /v1/health is
// polled by every lens and the hub, and one loopback request serves them all.
type proxyProbe struct {
	at  time.Time
	h   llmserve.Health
	err error
}

func (p *llmProxy) probed() proxyProbe {
	p.mu.Lock()
	defer p.mu.Unlock()
	if time.Since(p.probe.at) > 2*time.Second {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		h, err := p.client.Health(ctx)
		cancel()
		p.probe = proxyProbe{at: time.Now(), h: h, err: err}
	}
	return p.probe
}

// health is the "llmProxy" part of GET /v1/health. build-daemon.sh reads
// "mode" and "url" from it; rename neither without changing the script.
func (p *llmProxy) health() map[string]any {
	if p.unit == "" {
		return map[string]any{"mode": "in-process", "why": p.why}
	}
	probe := p.probed()
	p.mu.Lock()
	action, failed := p.action, p.failed
	p.mu.Unlock()
	out := map[string]any{
		"mode": "service", "unit": p.unit, "url": p.client.URL(),
		"serving":  probe.err == nil && probe.h.DB == p.db,
		"codeHash": probe.h.CodeHash, "wantHash": llmserve.CodeHash(),
		// What this start of the daemon did to the service ("" while it is
		// still deciding).
		"action": action,
	}
	switch {
	case probe.err != nil:
		out["error"] = probe.err.Error()
	case failed != "":
		out["error"] = failed
	}
	return out
}

// removeLLMProxyUnit takes the installed daemon's proxy service away with
// the daemon (ccmuxd uninstall). Only the default unit: one a daemon named
// for itself belongs to whoever named it.
func removeLLMProxyUnit() {
	unit := llmProxyUnitDefault + ".service"
	path := filepath.Join(systemdUserDir(), unit)
	if _, err := os.Stat(path); err != nil {
		return
	}
	_ = exec.Command("systemctl", "--user", "disable", "--now", unit).Run()
	if err := os.Remove(path); err != nil {
		fmt.Printf("  note: could not remove %s: %v\n", path, err)
		return
	}
	fmt.Printf("  removed systemd user service %s\n", unit)
}

// planProxyFor is planLLMProxy for the running daemon, and hands the manager
// the address panes are given. Before anything can make a pane: a pane keeps
// the address it starts with.
func planProxyFor(mgr *manager.Manager, unitFlag, addrFlag, daemonAddr, db string) *llmProxy {
	p := planLLMProxy(unitFlag, addrFlag, daemonAddr, db, thisProxyHost(runtime.GOOS))
	mgr.LLMProxyURL = p.URL()
	return p
}
