// Command ccmuxd is the ccmux daemon: it owns a dedicated tmux server that holds
// persistent Claude Code sessions and serves a REST + WebSocket API that lenses
// (native app, web, phone) attach to. With -tsnet it comes up as its own tailnet
// node (own name, own :443 with a tailnet-issued cert, in-process WhoIs identity);
// it always keeps a loopback listener for on-host hooks and health.
package main

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/gorilla/websocket"
	"tailscale.com/client/local"
	"tailscale.com/tsnet"

	"ccmux.dev/ccmuxd/config"
	"ccmux.dev/ccmuxd/internal/api"
	"ccmux.dev/ccmuxd/internal/devhost"
	"ccmux.dev/ccmuxd/internal/hooks"
	"ccmux.dev/ccmuxd/internal/hub"
	"ccmux.dev/ccmuxd/internal/manager"
	"ccmux.dev/ccmuxd/internal/peers"
	"ccmux.dev/ccmuxd/internal/push"
	"ccmux.dev/ccmuxd/internal/store"
	"ccmux.dev/ccmuxd/internal/tailnet"
	"ccmux.dev/ccmuxd/internal/tmux"
)

// main dispatches a leading non-flag arg (install/uninstall/upgrade/version)
// to the subcommand layer; otherwise it runs the daemon. The launchd/systemd
// invocation passes `-addr …` (starts with "-"), so it always lands on the
// daemon path — with one carve-out: `-version`/`--version` is what fingers
// type, and without the carve-out it would reach the daemon's flag parser and
// die with a usage dump instead of printing the version.
func main() {
	if len(os.Args) > 1 {
		if arg := os.Args[1]; arg == "-version" || arg == "--version" {
			runSubcommand("version", nil)
			return
		} else if !strings.HasPrefix(arg, "-") {
			runSubcommand(arg, os.Args[2:])
			return
		}
	}
	runDaemon()
}

func runDaemon() {
	socket := flag.String("socket", "ccmux", "tmux server socket name (-L)")
	addr := flag.String("addr", "127.0.0.1:7890", "HTTP listen address")
	dbPath := flag.String("db", defaultDBPath(), "registry SQLite path")
	hooksSock := flag.String("hooks-socket", "/tmp/ccmuxd-hooks.sock", "Claude Code hooks Unix socket (distinct from the native app's /tmp/ccmux-hooks.sock; injected into hosted panes as CCMUX_HOOKS_SOCK)")
	vapidPath := flag.String("vapid", defaultVAPIDPath(), "VAPID keypair JSON path (web push)")
	pushSubject := flag.String("push-subject", "https://ccmux.dev", "VAPID subject: a real contact email or https: URL identifying this server (Apple rejects unroutable domains like .local)")
	tsnetEnabled := flag.Bool("tsnet", false, "serve as an own tailnet node (HTTPS on :443, in-process WhoIs identity); needs TS_AUTHKEY on first run")
	tsnetHostname := flag.String("tsnet-hostname", "ccmuxd", "tailnet node name (→ <name>.<tailnet>.ts.net)")
	tsnetDir := flag.String("tsnet-dir", defaultTsnetDir(), "tsnet node state directory")
	projectsRoot := flag.String("projects-root", defaultProjectsRoot(), "folder whose subdirectories are offered as hosted-workspace locations (GET /v1/projects)")
	hubEnabled := flag.Bool("hub", false, "run hub-role services: aggregate every tag:ccmux host into one lens surface, own the peers bus + dev registrar + push (requires -tsnet)")
	flag.Parse()

	if *hubEnabled && !*tsnetEnabled {
		log.Fatal("--hub requires --tsnet (the hub discovers member hosts over the tailnet)")
	}

	cfgPath := filepath.Join(os.TempDir(), "ccmux-tmux.conf")
	if err := os.WriteFile(cfgPath, []byte(config.TmuxConf), 0o644); err != nil {
		log.Fatalf("write tmux config: %v", err)
	}
	if err := os.MkdirAll(filepath.Dir(*dbPath), 0o700); err != nil {
		log.Fatalf("mkdir db dir: %v", err)
	}
	st, err := store.Open(*dbPath)
	if err != nil {
		log.Fatalf("open registry: %v", err)
	}
	defer st.Close()

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	srv := &tmux.Server{Socket: *socket, ConfigPath: cfgPath}
	mgr := manager.New(ctx, srv, st)
	mgr.LocalURL = loopbackURL(*addr)
	mgr.HooksSocket = *hooksSock // hosted panes hit THIS path, not the app's

	// Built-in peers bus: pane env gets the bearer token (must be wired before
	// any pane is created), pane-less sessions discover url+token via the info
	// file. Non-fatal — the daemon serves terminals even if the bus can't come up.
	var peersSvc *peers.Service
	if secret, err := peers.LoadOrCreateSecret(filepath.Join(configDir(), "peers-secret")); err != nil {
		log.Printf("peers bus disabled: %v", err)
	} else {
		peersSvc = peers.NewService(st, mgr, secret)
		mgr.ExtraPaneEnv = peersSvc.PaneEnv
		// Claude Code's session hooks are the bus's only proof that a pane holds
		// a session that will read a message, rather than a claude process with
		// the MCP server merely loaded.
		mgr.SessionSink = peersSvc.NoteSession
		mgr.SessionLiveFn = peersSvc.PaneHasLiveSession
		peersSvc.Start(ctx)
	}

	mgr.StartReconciler() // retries attach; a cooled workspace must not stay dark
	if err := mgr.Start(); err != nil {
		log.Fatalf("manager start: %v", err)
	}
	// Daemon-side git dashboard (branch/ahead-behind/changed files) for every
	// live workspace — lenses render it; they can't read the daemon's repos.
	mgr.StartGitStatus(5 * time.Second)

	// Claude Code hooks → attention fan-out. The daemon owns a DISTINCT socket
	// (default /tmp/ccmuxd-hooks.sock) from the native app's /tmp/ccmux-hooks.sock,
	// and injects its path into hosted panes as CCMUX_HOOKS_SOCK — so hosted hooks
	// reach the daemon even when the app is running (no more last-binder-steals).
	// Still non-fatal if the socket is somehow taken; the daemon serves regardless.
	if hl, err := hooks.Listen(*hooksSock, mgr); err != nil {
		log.Printf("hooks listener disabled: %v", err)
	} else {
		defer hl.Close()
		log.Printf("hooks listening on %s", *hooksSock)
	}

	apiSrv := api.NewServer(mgr)
	apiSrv.SetProjectsRoot(*projectsRoot)
	if peersSvc != nil {
		apiSrv.EnablePeers(peersSvc)
		infoPath := filepath.Join(configDir(), "peers.json")
		if err := peers.WriteDaemonInfo(infoPath, loopbackURL(*addr), peersSvc.PanelessToken()); err != nil {
			log.Printf("peers daemon-info write failed (pane-less sessions won't connect): %v", err)
		} else {
			log.Printf("peers bus enabled (pane-less info %s)", infoPath)
		}
	}

	// Bring the tailnet node up FIRST (if enabled): hub mode must be set before
	// Handler() builds its routes (hub routes register conditionally) and before
	// push wires its notifier (the hub picks the federated variant).
	var ts *tsnet.Server
	var lc *local.Client
	if *tsnetEnabled {
		var err error
		ts, lc, err = setupTailnetNode(ctx, mgr, apiSrv, *tsnetHostname, *tsnetDir, *hubEnabled, peersSvc)
		if err != nil {
			log.Fatalf("tsnet: %v", err)
		}
		defer ts.Close()
	}

	// Web push: generate + persist a VAPID keypair on first run, then wire the
	// push endpoints + attention notifier. Non-fatal — the daemon still serves
	// terminals if push can't initialize.
	if keys, err := push.LoadOrCreateKeys(*vapidPath); err != nil {
		log.Printf("web push disabled: %v", err)
	} else {
		apiSrv.EnablePush(ctx, push.NewSender(keys, *pushSubject), st)
		log.Printf("web push enabled (vapid %s)", *vapidPath)
	}

	handler := apiSrv.Handler()

	// Serve the tailnet node's HTTPS :443 now that the handler is built. Dev
	// hostnames ride the same listener: the devhost server wraps the handler (Host
	// dispatch) and TLS config (SNI dispatch). The loopback listener below gets the
	// same dispatch (harmless, testable).
	if *tsnetEnabled {
		dh, err := serveTailnetHTTPS(ctx, ts, lc, mgr, apiSrv, handler, *tsnetHostname)
		if err != nil {
			log.Fatalf("tsnet serve: %v", err)
		}
		handler = dh.Handler(handler)
	}

	httpSrv := &http.Server{Addr: *addr, Handler: handler}
	go func() {
		<-ctx.Done()
		shutCtx, c := context.WithTimeout(context.Background(), 3e9)
		defer c()
		_ = httpSrv.Shutdown(shutCtx)
	}()

	log.Printf("ccmuxd loopback listening on http://%s (tmux -L %s, db %s)", *addr, *socket, *dbPath)
	if err := httpSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatalf("http: %v", err)
	}
	log.Print("ccmuxd stopped")
}

// serveTailnet brings the daemon up as its own tailnet node, swaps the API's
// identity backend to the node's in-process WhoIs, wires the devhost server,
// and serves over HTTPS on the node's :443 (background). TLS certs dispatch by
// SNI — dev-domain names get the certmagic wildcard, the node's own name its
// ts.net cert — and requests dispatch by Host. The auth key comes from
// TS_AUTHKEY (or prior persisted state in dir); first unauthenticated run logs
// a login URL.
// setupTailnetNode brings the daemon up as its own tailnet node, swaps the API's
// identity backend to the node's in-process WhoIs, and enables the hub role (or
// host-side peers federation). This runs BEFORE the HTTP handler is built, so
// hub-conditional routes register and push picks the federated notifier. The auth
// key comes from TS_AUTHKEY (or prior persisted state in dir); a first
// unauthenticated run logs a login URL.
func setupTailnetNode(ctx context.Context, mgr *manager.Manager, apiSrv *api.Server, hostname, dir string, hubEnabled bool, peersSvc *peers.Service) (*tsnet.Server, *local.Client, error) {
	ts := &tsnet.Server{Hostname: hostname, Dir: dir, UserLogf: log.Printf}
	if _, err := ts.Up(ctx); err != nil {
		return nil, nil, fmt.Errorf("node up: %w", err)
	}
	lc, err := ts.LocalClient()
	if err != nil {
		ts.Close()
		return nil, nil, fmt.Errorf("local client: %w", err)
	}
	apiSrv.SetIdentityResolver(tailnet.NewLocalResolver(lc))

	// Hub role: aggregate every tag:ccmux member host. Non-fatal — if the node
	// status isn't ready we log and serve host-only.
	if hubEnabled {
		if err := enableHub(ctx, ts, lc, mgr, apiSrv, peersSvc); err != nil {
			log.Printf("hub mode disabled: %v", err)
		}
	} else {
		enableHostFederation(ctx, ts, lc, mgr, peersSvc)
	}
	return ts, lc, nil
}

// serveTailnetHTTPS serves the built handler over the node's :443 with a
// tailnet-issued cert, wiring the devhost server (SNI + Host dispatch for dev
// hostnames). Returns the devhost server so the loopback listener gets the same
// dispatch.
func serveTailnetHTTPS(ctx context.Context, ts *tsnet.Server, lc *local.Client, mgr *manager.Manager, apiSrv *api.Server, handler http.Handler, hostname string) (*devhost.Server, error) {
	ip4, _ := ts.TailscaleIPs()
	dh := devhost.NewServer(ctx, mgr, filepath.Join(configDir(), "devhost"), tsSuffix(ts, hostname), ip4)
	mgr.OnDevhostChange = dh.Refresh
	apiSrv.SetDevhostStatus(dh.CertStatus)
	dh.Refresh()
	dh.StartProbe(5 * time.Second)

	rawLn, err := ts.Listen("tcp", ":443")
	if err != nil {
		return nil, fmt.Errorf("listen: %w", err)
	}
	// lc.GetCertificate fails unless HTTPS certs are enabled in the tailnet
	// admin console — same requirement the old ListenTLS carried.
	ln := tls.NewListener(rawLn, dh.TLSConfig(lc.GetCertificate))
	log.Printf("tsnet node up: %s (ip %s), https on the tailnet, cert domains %v", hostname, ip4, ts.CertDomains())
	// In hub mode, WrapDevhost routes a dev-hostname owned by another host to it
	// before local devhost dispatch (no-op off the hub).
	served := apiSrv.WrapDevhost(dh.Handler(handler))
	go func() {
		if err := http.Serve(ln, served); err != nil && err != http.ErrServerClosed {
			log.Printf("tsnet serve stopped: %v", err)
		}
	}()
	return dh, nil
}

// enableHub wires the hub-role services onto the API server: a member registry
// discovered from the tailnet (tag:ccmux peers + self), a workspace aggregator
// over a shared tailnet-dialing transport, and the periodic health probe. selfID
// is the hub node's MagicDNS label, read from its own tailnet status.
func enableHub(ctx context.Context, ts *tsnet.Server, lc *local.Client, mgr *manager.Manager, apiSrv *api.Server, peersSvc *peers.Service) error {
	st, err := lc.Status(ctx)
	if err != nil {
		return fmt.Errorf("node status: %w", err)
	}
	if st.Self == nil || st.Self.DNSName == "" {
		return fmt.Errorf("node has no MagicDNS name yet")
	}
	selfID := firstLabel(st.Self.DNSName)

	transport := &http.Transport{DialContext: ts.Dial}
	probeClient := &http.Client{Transport: transport}
	reg := hub.NewRegistry(selfID, hub.DefaultFloor,
		hub.TailnetDiscoverer(ctx, lc),
		hub.HTTPProber(probeClient, 3*time.Second),
		func() int64 { return time.Now().UnixMilli() },
	)
	client := hub.NewClient(transport)
	agg := hub.NewAggregator(selfID, reg, mgr, client.Workspaces)
	reg.StartProbe(ctx, 5*time.Second)
	agg.StartRefresh(ctx, 5*time.Second) // keep the pane→group/host indexes fresh for peers

	// Peers federation (hub-side): resolve a remote pane's window group across all
	// hosts and stamp its owning-host label. Cached-map reads only — no I/O under
	// the bus lock. The remote-connection auth relaxation + host env redirect that
	// actually route cross-host panes here are the live-test-gated follow-up.
	if peersSvc != nil {
		peersSvc.EnableFederation(agg.GroupForPane, agg.Owner)
	}

	// WebSocket dialer for the event firehose fan-in — same tailnet dial as the
	// REST transport so wss://<host>.ts.net/v1/events resolves and validates.
	wsDialer := &websocket.Dialer{NetDialContext: ts.Dial, HandshakeTimeout: 10 * time.Second}
	wsDial := func(dctx context.Context, urlStr string) (*websocket.Conn, error) {
		conn, _, err := wsDialer.DialContext(dctx, urlStr, nil)
		return conn, err
	}

	apiSrv.EnableHub(reg, agg, client, selfID, wsDial)
	log.Printf("hub mode: self=%s, discovering %s peers", selfID, hub.CcmuxTag)
	return nil
}

// enableHostFederation points a non-hub host's Claude panes at the hub's peers
// bus (discovered via tag:ccmux-hub), so they join the hub's directory and can
// message peers on other hosts. It overrides ExtraPaneEnv to inject
// CCMUX_PEERS_URL (the hub) + a hub-minted CCMUX_PANE_TOKEN, leaving
// CCMUX_DAEMON_URL local so on-host hooks still reach this host. Discovery runs
// in the background (the hub may start later); until a hub is found, and whenever
// the hub can't be reached at spawn, panes fall back to the LOCAL bus — so a
// single-host / no-hub node behaves exactly as before. No secret is distributed;
// the hub mints each token.
func enableHostFederation(ctx context.Context, ts *tsnet.Server, lc *local.Client, mgr *manager.Manager, peersSvc *peers.Service) {
	if peersSvc == nil {
		return
	}
	var hubURL atomic.Pointer[string]
	empty := ""
	hubURL.Store(&empty)
	go func() {
		t := time.NewTicker(15 * time.Second)
		defer t.Stop()
		for {
			if st, err := lc.Status(ctx); err == nil && st.Self != nil {
				if u := hub.DiscoverHub(ctx, lc, firstLabel(st.Self.DNSName)); u != "" && u != *hubURL.Load() {
					hubURL.Store(&u)
					log.Printf("peers: federating to hub %s", u)
				}
			}
			select {
			case <-ctx.Done():
				return
			case <-t.C:
			}
		}
	}()

	client := &http.Client{Transport: &http.Transport{DialContext: ts.Dial}, Timeout: 3 * time.Second}
	localEnv := peersSvc.PaneEnv // fallback: local bus when there's no hub / it's unreachable
	mgr.ExtraPaneEnv = func(paneID string) map[string]string {
		u := *hubURL.Load()
		if u == "" {
			return localEnv(paneID)
		}
		token, err := mintHubPaneToken(client, u, paneID)
		if err != nil {
			log.Printf("peers: hub mint failed for pane %s (%v) — using local bus", paneID, err)
			return localEnv(paneID)
		}
		return map[string]string{"CCMUX_PEERS_URL": u, "CCMUX_PANE_TOKEN": token}
	}
	log.Printf("peers: host federation armed (discovering %s)", hub.HubTag)
}

// mintHubPaneToken asks the hub to mint a pane's bus token (POST /v1/peers/pane-token).
func mintHubPaneToken(client *http.Client, hubURL, paneID string) (string, error) {
	body, _ := json.Marshal(map[string]string{"pane_id": paneID})
	req, err := http.NewRequest(http.MethodPost, hubURL+"/v1/peers/pane-token", bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("mint HTTP %d", resp.StatusCode)
	}
	var out struct {
		Token string `json:"token"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", err
	}
	if out.Token == "" {
		return "", fmt.Errorf("hub returned empty token")
	}
	return out.Token, nil
}

// firstLabel returns the first DNS label of a MagicDNS FQDN (trailing-dot safe).
func firstLabel(dnsName string) string {
	name := strings.TrimSuffix(dnsName, ".")
	if i := strings.IndexByte(name, '.'); i >= 0 {
		return name[:i]
	}
	return name
}

// tsSuffix derives the tailnet's MagicDNS suffix (e.g. tailb9053d.ts.net) from
// the node's cert domain, for fallback-mode URLs.
func tsSuffix(ts *tsnet.Server, hostname string) string {
	for _, d := range ts.CertDomains() {
		if s, ok := strings.CutPrefix(d, hostname+"."); ok {
			return s
		}
	}
	return "ts.net" // unreachable in practice; keeps URLs recognizably wrong, not empty
}

// loopbackURL turns a listen address into a base URL an on-host hook can reach.
// A wildcard/empty host (0.0.0.0, ::, "") isn't dialable as a client, so it maps
// to 127.0.0.1; a concrete host is kept as-is.
func loopbackURL(addr string) string {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return ""
	}
	if host == "" || host == "0.0.0.0" || host == "::" {
		host = "127.0.0.1"
	}
	return "http://" + net.JoinHostPort(host, port)
}

// defaultDBPath returns ~/Library/Application Support/ccmuxd/ccmuxd.db (or the
// XDG/OS equivalent on the future Linux host).
func defaultDBPath() string { return filepath.Join(configDir(), "ccmuxd.db") }

// defaultVAPIDPath returns the VAPID keypair path beside the registry.
func defaultVAPIDPath() string { return filepath.Join(configDir(), "vapid.json") }

// defaultTsnetDir returns the tsnet node's state directory beside the registry.
func defaultTsnetDir() string { return filepath.Join(configDir(), "tsnet") }

// defaultProjectsRoot is the daemon user's home — a server deployment narrows it
// with -projects-root (e.g. /srv/projects).
func defaultProjectsRoot() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return home
}

func configDir() string {
	dir, err := os.UserConfigDir()
	if err != nil {
		dir = os.TempDir()
	}
	return filepath.Join(dir, "ccmuxd")
}
