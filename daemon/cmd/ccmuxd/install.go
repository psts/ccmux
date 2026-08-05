// Subcommand layer: `ccmuxd install` / `ccmuxd uninstall`. The daemon binary
// installs itself as a user service (launchd on macOS, systemd --user on Linux),
// so a fresh host needs only the binary + this command — no separate script that
// could drift from the real flags. The OS-specific service writers live in
// service_darwin.go / service_linux.go (build-tagged); everything portable is here.
package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"tailscale.com/tsnet"

	"ccmux.dev/ccmuxd/internal/version"
)

// serviceConfig is the portable description of the daemon invocation a service
// wraps. The OS-specific writers (service_darwin.go / service_linux.go) turn it
// into a launchd plist or a systemd unit.
type serviceConfig struct {
	BinPath    string            // absolute path to the ccmuxd binary
	Args       []string          // daemon flags (e.g. -addr … -tsnet …)
	Env        map[string]string // extra environment beyond the writer's PATH default
	WorkingDir string
}

// minTmux is the conservative floor the daemon's tmux.conf needs:
// allow-passthrough (3.3) is the newest option it sets. Bump only if the config
// starts using a younger feature.
const minTmux = "3.3"

// runSubcommand dispatches a leading non-flag arg to a verb and exits the
// process. Anything starting with "-" (e.g. the launchd/systemd `-addr …`
// invocation) never reaches here, so the daemon path is untouched.
func runSubcommand(name string, args []string) {
	var err error
	switch name {
	case "install":
		err = cmdInstall(args)
	case "uninstall":
		err = cmdUninstall(args)
	case "upgrade":
		err = cmdUpgrade(args)
	case "version": // -version/--version arrive here via main's carve-out
		fmt.Printf("ccmuxd %s (wire contract %d)\n", version.Build, version.Contract)
		os.Exit(0)
	default:
		fmt.Fprintf(os.Stderr, "ccmuxd: unknown command %q\n\n", name)
		printUsage()
		os.Exit(2)
	}
	if errors.Is(err, flag.ErrHelp) {
		return // flag already printed usage
	}
	if err != nil {
		log.Fatalf("%s: %v", name, err)
	}
}

func printUsage() {
	fmt.Fprint(os.Stderr, `ccmuxd — the ccmux daemon

Usage:
  ccmuxd [flags]        run the daemon (see -h)
  ccmuxd install [opts] install + start ccmuxd as a user service
  ccmuxd upgrade [vX.Y.Z] self-update to the latest (or named) release and restart
  ccmuxd uninstall      stop + remove the service (-purge also wipes state)
  ccmuxd version        print the build version
`)
}

// installOpts is the resolved install configuration, filled from flags then any
// missing pieces prompted interactively.
type installOpts struct {
	Addr         string
	Hostname     string
	ProjectsRoot string
	AuthKey      string
	Tsnet        bool
	Hub          bool
	RegisterMCP  bool
}

// parseInstallFlags also reports WHICH flags the caller set explicitly, so an
// update can fill the rest from the previous install without overriding a
// deliberate change.
func parseInstallFlags(args []string) (*installOpts, map[string]bool, error) {
	fs := flag.NewFlagSet("install", flag.ContinueOnError)
	o := &installOpts{}
	fs.StringVar(&o.Addr, "addr", "127.0.0.1:7900", "loopback HTTP listen address")
	fs.StringVar(&o.Hostname, "hostname", "", "tailnet node name (default: this machine's hostname)")
	fs.StringVar(&o.ProjectsRoot, "projects-root", "", "folder offered as hosted-workspace locations (default: home)")
	// Default empty (NOT os.Getenv): flag echoes defaults in -h, and the auth key
	// is a secret. The env fallback is applied after parsing instead.
	fs.StringVar(&o.AuthKey, "authkey", "", "Tailscale auth key for the first-run join (or set TS_AUTHKEY)")
	noTsnet := fs.Bool("no-tsnet", false, "install a purely local daemon (no tailnet node)")
	fs.BoolVar(&o.Hub, "hub", false, "run the hub role (aggregates every tag:ccmux host; exactly one per fleet)")
	fs.BoolVar(&o.RegisterMCP, "register-peers-mcp", true, "register the ccmux-peers MCP server for Claude Code on this host")
	if err := fs.Parse(args); err != nil {
		return nil, nil, err
	}
	set := map[string]bool{}
	fs.Visit(func(f *flag.Flag) { set[f.Name] = true })
	if o.AuthKey == "" {
		o.AuthKey = os.Getenv("TS_AUTHKEY")
	}
	o.Tsnet = !*noTsnet
	// The hub-requires-tsnet invariant is checked in cmdInstall, AFTER the
	// previous install's answers are merged in — checking here misses the merge
	// re-creating the combination (e.g. explicit -hub onto a saved no-tsnet
	// install), which would silently drop the hub role.
	return o, set, nil
}

// cmdInstall is the whole install flow: preflight → resolve config → join the
// tailnet once → write+start the service → register the peers MCP → verify.
//
// A host that was installed before is UPDATED, not re-interviewed: the previous
// answers (install.json, or recovered from the existing service file) fill
// every option not explicitly flagged, and no prompts run. Re-prompting used to
// offer the machine's OS hostname as the default node name — accepting it on an
// update would have renamed the tailnet node.
func cmdInstall(args []string) error {
	// Say which build is doing the installing, first thing — "which version did
	// I just get?" must be answerable from the curl|sh output alone.
	fmt.Printf("  ccmuxd %s\n", version.Build)
	o, set, err := parseInstallFlags(args)
	if err != nil {
		return err
	}
	if err := checkPrereqs(); err != nil {
		return err
	}
	self, err := resolveSelf()
	if err != nil {
		return err
	}
	if prev, src := loadPreviousInstall(); prev != nil {
		applySaved(o, prev, set)
		if o.Hostname == "" {
			o.Hostname = defaultHostLabel()
		}
		fmt.Printf("  updating existing install (config from %s): hostname=%s hub=%v — pass flags to change\n",
			src, o.Hostname, o.Hub)
	} else {
		t := openTTY()
		defer t.close()
		fillInteractive(o, t)
	}
	if o.Hub && !o.Tsnet {
		return fmt.Errorf("--hub requires the tailnet (drop --no-tsnet, or re-run with -hub=false)")
	}

	if o.Tsnet {
		if err := ensureTailnetAuth(o); err != nil {
			return fmt.Errorf("tailnet join: %w", err)
		}
	}
	if err := writeAndStartService(buildServiceConfig(o, self)); err != nil {
		return err
	}
	saveInstallConfig(o)
	if o.RegisterMCP {
		registerPeersMCP(filepath.Join(filepath.Dir(self), "ccmux-peers"))
	}
	reportHealthAndNext(o)
	return nil
}

func cmdUninstall(args []string) error {
	fs := flag.NewFlagSet("uninstall", flag.ContinueOnError)
	purge := fs.Bool("purge", false, "also delete the daemon's state (registry DB, tailnet node, keys)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	return stopAndRemoveService(*purge)
}

// resolveSelf returns the absolute, symlink-resolved path of the running ccmuxd
// binary — what the service's ExecStart points at, and where ccmux-peers sits
// beside it.
func resolveSelf() (string, error) {
	self, err := os.Executable()
	if err != nil {
		return "", err
	}
	if resolved, err := filepath.EvalSymlinks(self); err == nil {
		self = resolved
	}
	return self, nil
}

// checkPrereqs fails fast if the daemon's runtime deps are missing: it shells out
// to git and tmux, and tmux must be new enough for the config it loads.
func checkPrereqs() error {
	if _, err := exec.LookPath("git"); err != nil {
		return fmt.Errorf("git not found on PATH — %s", installHint("git"))
	}
	tmuxBin, err := exec.LookPath("tmux")
	if err != nil {
		return fmt.Errorf("tmux not found on PATH — %s", installHint("tmux"))
	}
	out, err := exec.Command(tmuxBin, "-V").Output()
	if err != nil {
		return fmt.Errorf("tmux -V failed: %w", err)
	}
	if ok, ver := tmuxAtLeast(string(out), minTmux); !ok {
		return fmt.Errorf("tmux %s is too old; need >= %s — %s", ver, minTmux, installHint("tmux"))
	}
	return nil
}

// tmuxAtLeast parses `tmux -V` output ("tmux 3.6b") and compares major.minor
// against min. An unparseable version is allowed (returns true) rather than
// blocking a working host on a format we didn't anticipate.
func tmuxAtLeast(vout, min string) (bool, string) {
	f := strings.Fields(strings.TrimSpace(vout))
	if len(f) < 2 {
		return true, strings.TrimSpace(vout)
	}
	ver := f[1]
	aM, aN := parseVer(ver)
	if aM == 0 {
		return true, ver // no leading digit found — don't block
	}
	bM, bN := parseVer(min)
	if aM != bM {
		return aM > bM, ver
	}
	return aN >= bN, ver
}

// parseVer pulls leading major.minor ints out of a version string, skipping any
// non-digit prefix ("next-3.7") and ignoring a suffix ("3.6b").
func parseVer(s string) (int, int) {
	for len(s) > 0 && (s[0] < '0' || s[0] > '9') {
		s = s[1:]
	}
	i := 0
	for i < len(s) && s[i] >= '0' && s[i] <= '9' {
		i++
	}
	maj, _ := strconv.Atoi(s[:i])
	minr := 0
	if i < len(s) && s[i] == '.' {
		j := i + 1
		for j < len(s) && s[j] >= '0' && s[j] <= '9' {
			j++
		}
		minr, _ = strconv.Atoi(s[i+1 : j])
	}
	return maj, minr
}

// fillInteractive prompts (on /dev/tty, so it works even under `curl … | sh`
// where stdin is the script) for anything the flags left blank.
func fillInteractive(o *installOpts, t *tty) {
	if o.Hostname == "" {
		o.Hostname = defaultHostLabel()
		if t != nil {
			o.Hostname = t.ask("Tailnet node name", o.Hostname)
		}
	}
	if !o.Tsnet {
		return
	}
	if o.AuthKey == "" && !tailnetStateExists() && t != nil {
		o.AuthKey = t.ask("Tailscale auth key (blank = browser login on first start)", "")
	}
	if !o.Hub && t != nil {
		o.Hub = t.askBool("Run the hub role on this host?", false)
	}
}

// ensureTailnetAuth joins the tailnet once, here, so the auth key is consumed
// into persisted node state and never has to live in the service file. Skips if
// already joined (a re-install) or if no key was given (falls back to the
// daemon's browser-login-on-first-start).
func ensureTailnetAuth(o *installOpts) error {
	if tailnetStateExists() {
		return nil
	}
	if o.AuthKey == "" {
		fmt.Println("  no auth key given — ccmuxd will log a login URL on first start; open it to authenticate.")
		return nil
	}
	fmt.Println("  joining the tailnet (first run)…")
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	ts := &tsnet.Server{Hostname: o.Hostname, Dir: defaultTsnetDir(), AuthKey: o.AuthKey, UserLogf: log.Printf}
	defer ts.Close() // release the state-dir lock on either path, before the daemon starts
	_, err := ts.Up(ctx)
	return err
}

// buildServiceConfig turns the resolved options into the exact daemon invocation
// the service will run. TS_AUTHKEY is intentionally absent from Env: the join
// already happened in ensureTailnetAuth.
func buildServiceConfig(o *installOpts, self string) serviceConfig {
	args := []string{"-addr", o.Addr}
	if o.Tsnet {
		args = append(args, "-tsnet", "-tsnet-hostname", o.Hostname)
		if o.Hub {
			args = append(args, "-hub")
		}
	}
	if o.ProjectsRoot != "" {
		args = append(args, "-projects-root", o.ProjectsRoot)
	}
	home, _ := os.UserHomeDir()
	return serviceConfig{BinPath: self, Args: args, WorkingDir: home}
}

// registerPeersMCP wires the per-host ccmux-peers thin-client into Claude Code's
// user-scope MCP config, so panes on THIS host can reach the bus. Idempotent:
// removes any stale entry first. If the claude CLI or the binary is absent, it
// prints the command for the user to run.
func registerPeersMCP(peersPath string) {
	cmd := fmt.Sprintf("claude mcp add --scope user --transport stdio claude-peers -- %s", peersPath)
	if _, err := os.Stat(peersPath); err != nil {
		fmt.Printf("  ccmux-peers not found beside ccmuxd (%s); skipping MCP registration\n", peersPath)
		return
	}
	if _, err := exec.LookPath("claude"); err != nil {
		fmt.Printf("  claude CLI not found; register the peers bus yourself:\n    %s\n", cmd)
		return
	}
	_ = exec.Command("claude", "mcp", "remove", "--scope", "user", "claude-peers").Run() // ignore: may not exist
	add := exec.Command("claude", "mcp", "add", "--scope", "user", "--transport", "stdio", "claude-peers", "--", peersPath)
	if out, err := add.CombinedOutput(); err != nil {
		fmt.Printf("  peers MCP registration failed (%v); run it yourself:\n    %s\n    %s\n", err, cmd, strings.TrimSpace(string(out)))
		return
	}
	fmt.Printf("  registered claude-peers MCP → %s\n", peersPath)
}

// reportHealthAndNext confirms the daemon answered on its loopback and prints the
// one step that can't be scripted: tagging the node in the Tailscale console.
func reportHealthAndNext(o *installOpts) {
	if v, c, err := verifyHealth(o.Addr); err != nil {
		fmt.Printf("  started, but health check failed: %v\n", err)
	} else {
		fmt.Printf("  healthy: ccmuxd %s (contract %d) on http://%s\n", v, c, o.Addr)
	}
	if !o.Tsnet {
		return
	}
	fmt.Println("\nNext (one manual step): in the Tailscale admin console, tag this node")
	if o.Hub {
		fmt.Println("  tag:ccmux  and  tag:ccmux-hub")
	} else {
		fmt.Println("  tag:ccmux")
	}
	fmt.Println("Then the hub discovers it automatically within one probe cycle.")
}

// verifyHealth polls the daemon's loopback /v1/health until it answers ok or the
// deadline passes, returning the reported build + wire contract.
func verifyHealth(addr string) (string, int, error) {
	url := loopbackURL(addr) + "/v1/health"
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		if v, c, ok := probeHealth(url); ok {
			return v, c, nil
		}
		time.Sleep(500 * time.Millisecond)
	}
	return "", 0, fmt.Errorf("no healthy response from %s within 20s", url)
}

func probeHealth(url string) (string, int, bool) {
	resp, err := http.Get(url)
	if err != nil {
		return "", 0, false
	}
	defer resp.Body.Close()
	var h struct {
		OK       bool   `json:"ok"`
		Version  string `json:"version"`
		Contract int    `json:"contract"`
	}
	if json.NewDecoder(resp.Body).Decode(&h) != nil || !h.OK {
		return "", 0, false
	}
	return h.Version, h.Contract, true
}

// purgeState removes the daemon's whole config/state tree (registry DB, tsnet
// node, VAPID + peers secrets). Called only by `uninstall -purge`.
func purgeState() {
	dir := configDir()
	if err := os.RemoveAll(dir); err != nil {
		fmt.Printf("  could not remove state %s: %v\n", dir, err)
		return
	}
	fmt.Printf("  purged state %s\n", dir)
}

// defaultHostLabel is this machine's short hostname, lowercased — a sensible
// default tailnet node name.
func defaultHostLabel() string {
	h, err := os.Hostname()
	if err != nil || h == "" {
		return "ccmuxd"
	}
	if i := strings.IndexByte(h, '.'); i > 0 {
		h = h[:i]
	}
	return strings.ToLower(h)
}

// tailnetStateExists reports whether this host already joined the tailnet, so a
// re-install skips the auth-key step and doesn't fight the daemon for the node.
func tailnetStateExists() bool {
	_, err := os.Stat(filepath.Join(defaultTsnetDir(), "tailscaled.state"))
	return err == nil
}

// tty is a minimal /dev/tty prompt helper. It reads the controlling terminal
// directly so prompts work under `curl … | sh`, where stdin is the pipe.
type tty struct {
	r *bufio.Reader
	w *os.File
}

func openTTY() *tty {
	f, err := os.OpenFile("/dev/tty", os.O_RDWR, 0)
	if err != nil {
		return nil
	}
	return &tty{r: bufio.NewReader(f), w: f}
}

func (t *tty) close() {
	if t != nil {
		t.w.Close()
	}
}

func (t *tty) ask(label, def string) string {
	if def != "" {
		fmt.Fprintf(t.w, "%s [%s]: ", label, def)
	} else {
		fmt.Fprintf(t.w, "%s: ", label)
	}
	line, _ := t.r.ReadString('\n')
	if line = strings.TrimSpace(line); line != "" {
		return line
	}
	return def
}

func (t *tty) askBool(label string, def bool) bool {
	hint := "y/N"
	if def {
		hint = "Y/n"
	}
	switch strings.ToLower(t.ask(label+" ("+hint+")", "")) {
	case "y", "yes":
		return true
	case "n", "no":
		return false
	default:
		return def
	}
}
