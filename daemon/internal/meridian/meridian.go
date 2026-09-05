// Package meridian supervises Meridian sidecars: one local process per
// "meridian"-kind LLM account. Meridian is a community proxy that speaks the
// Anthropic Messages API and forwards through the Claude Agent SDK, which is
// the only path that lets a non-Claude-Code harness (opencode, pi) spend a
// Claude subscription — Anthropic refuses subscription tokens from
// third-party callers server-side, so the daemon's own proxy cannot do it.
//
// The daemon owns the process the way it owns dev servers: start it when the
// account exists, restart it with backoff when it dies, stop it when the
// account goes away. The account's stored key is the Claude setup token; it
// reaches Meridian only as CLAUDE_CODE_OAUTH_TOKEN in the child's environment
// and never travels in a request.
package meridian

import (
	"context"
	"fmt"
	"net"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"ccmux.dev/ccmuxd/internal/llmproxy"
)

// Kind is the llm account kind a Meridian sidecar serves.
const Kind = llmproxy.KindMeridian

// DefaultBaseURL is where Meridian listens when the account names no URL.
const DefaultBaseURL = llmproxy.MeridianUpstream

// Spec is what one sidecar needs: which account it serves, where to listen,
// and the subscription token the SDK inside it authenticates with.
type Spec struct {
	Name    string
	BaseURL string
	Token   string
}

// SpecsFrom picks the sidecars the configured accounts call for: one per
// meridian-kind account, its stored key being the token the sidecar runs on.
func SpecsFrom(accs []llmproxy.Account) []Spec {
	var out []Spec
	for _, a := range accs {
		if a.Kind == Kind {
			out = append(out, Spec{Name: a.Name, BaseURL: a.BaseURL, Token: a.APIKey})
		}
	}
	return out
}

// Status is the lens-facing state of one sidecar.
type Status struct {
	Running   bool      `json:"running"`
	PID       int       `json:"pid,omitempty"`
	Since     time.Time `json:"since,omitempty"`
	Restarts  int       `json:"restarts"`
	LastError string    `json:"lastError,omitempty"`
}

// Listen splits an account BaseURL into the host and port Meridian binds.
// Only loopback hosts are accepted: the token inside the process must not be
// reachable from the tailnet.
func Listen(baseURL string) (host string, port int, err error) {
	if baseURL == "" {
		baseURL = DefaultBaseURL
	}
	u, err := url.Parse(baseURL)
	if err != nil || u.Scheme != "http" || !loopback(u.Hostname()) {
		return "", 0, fmt.Errorf("meridian: base URL %q must be http://<loopback>:port", baseURL)
	}
	// The port is the one thing the proxy and the sidecar must agree on, so
	// it is never guessed: a URL without one would forward to :80 while the
	// sidecar listened on a default.
	if port, err = strconv.Atoi(u.Port()); err != nil {
		return "", 0, fmt.Errorf("meridian: base URL %q needs an explicit port", baseURL)
	}
	return u.Hostname(), port, nil
}

func loopback(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// Command builds the child command for one spec. Exported for the test and
// for `ccmuxd doctor`-style inspection; the supervisor calls it on every
// (re)start so a token rotated through settings applies on the next restart.
func Command(ctx context.Context, bin string, s Spec) (*exec.Cmd, error) {
	host, port, err := Listen(s.BaseURL)
	if err != nil {
		return nil, err
	}
	cmd := exec.CommandContext(ctx, bin)
	cmd.Env = append(os.Environ(),
		"PATH="+childPath(bin),
		"CLAUDE_CODE_OAUTH_TOKEN="+s.Token,
		"MERIDIAN_HOST="+host,
		"MERIDIAN_PORT="+strconv.Itoa(port),
		"MERIDIAN_PASSTHROUGH=1",
	)
	return cmd, nil
}

// childPath is the PATH the sidecar runs with. The daemon lives under systemd
// with the bare system PATH, and `meridian` is a node script whose shebang is
// `/usr/bin/env node`: without node on PATH it dies with exit 127 (seen live
// 2026-09-05). npm installs the script under <prefix>/lib/node_modules and
// node itself under <prefix>/bin, so the resolved symlink target names the
// directory node is in. ~/.local/bin is added for the same reason harness
// detection looks there.
func childPath(bin string) string {
	parts := []string{}
	if real, err := filepath.EvalSymlinks(bin); err == nil {
		if i := strings.Index(real, "/lib/node_modules/"); i > 0 {
			parts = append(parts, filepath.Join(real[:i], "bin"))
		}
	}
	if home, err := os.UserHomeDir(); err == nil {
		parts = append(parts, filepath.Join(home, ".local", "bin"))
	}
	return strings.Join(append(parts, os.Getenv("PATH")), string(os.PathListSeparator))
}

// PluginPath returns the opencode plugin file shipped inside the Meridian npm
// package, resolved from the binary on PATH (bin/meridian -> dist/cli.js is a
// symlink into the package), or "" when Meridian is not installed. opencode
// panes list it in their config so Meridian can tell opencode's own
// title/summary agents from the primary agent.
func PluginPath(lookPath func(string) (string, error)) string {
	bin, err := lookPath("meridian")
	if err != nil {
		return ""
	}
	real, err := filepath.EvalSymlinks(bin)
	if err != nil {
		return ""
	}
	// <pkg>/dist/cli.js -> <pkg>/plugin/meridian.ts
	p := filepath.Join(filepath.Dir(filepath.Dir(real)), "plugin", "meridian.ts")
	if _, err := os.Stat(p); err != nil {
		return ""
	}
	return p
}

// Supervisor keeps one running Meridian per spec handed to Reconcile.
type Supervisor struct {
	ctx context.Context
	bin func() (string, error)

	mu    sync.Mutex
	procs map[string]*proc
}

// New returns a supervisor that finds the meridian binary with lookPath. The
// daemon runs under systemd's bare PATH, so callers pass a lookup that also
// checks ~/.local/bin (harness.LookPath).
func New(ctx context.Context, lookPath func(string) (string, error)) *Supervisor {
	return &Supervisor{
		ctx:   ctx,
		bin:   func() (string, error) { return lookPath("meridian") },
		procs: map[string]*proc{},
	}
}

// Reconcile makes the running set match specs: new names start, vanished
// names stop, a name whose spec changed restarts. Safe to call on every
// settings apply.
func (sv *Supervisor) Reconcile(specs []Spec) {
	sv.mu.Lock()
	defer sv.mu.Unlock()
	want := map[string]Spec{}
	for _, s := range specs {
		want[s.Name] = s
	}
	for name, p := range sv.procs {
		if s, ok := want[name]; !ok || s != p.spec {
			p.stop()
			delete(sv.procs, name)
		}
	}
	for name, s := range want {
		if _, ok := sv.procs[name]; ok {
			continue
		}
		p := newProc(sv.ctx, sv.bin, s)
		sv.procs[name] = p
		go p.run()
	}
}

// Status reports every supervised sidecar by account name.
func (sv *Supervisor) Status() map[string]Status {
	sv.mu.Lock()
	defer sv.mu.Unlock()
	out := make(map[string]Status, len(sv.procs))
	for name, p := range sv.procs {
		out[name] = p.status()
	}
	return out
}

// Stop ends every sidecar; used at daemon shutdown.
func (sv *Supervisor) Stop() {
	sv.Reconcile(nil)
}
