// Command ccmuxd is the ccmux daemon: it owns a dedicated tmux server that holds
// persistent Claude Code sessions and serves a REST + WebSocket API that lenses
// (native app, web, phone) attach to. With -tsnet it comes up as its own tailnet
// node (own name, own :443 with a tailnet-issued cert, in-process WhoIs identity);
// it always keeps a loopback listener for on-host hooks and health.
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"tailscale.com/tsnet"

	"ccmux.dev/ccmuxd/config"
	"ccmux.dev/ccmuxd/internal/api"
	"ccmux.dev/ccmuxd/internal/hooks"
	"ccmux.dev/ccmuxd/internal/manager"
	"ccmux.dev/ccmuxd/internal/push"
	"ccmux.dev/ccmuxd/internal/store"
	"ccmux.dev/ccmuxd/internal/tailnet"
	"ccmux.dev/ccmuxd/internal/tmux"
)

func main() {
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
	flag.Parse()

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

	// Own tailnet node: HTTPS on its node's :443 (tailnet cert) with in-process
	// WhoIs identity, replacing `tailscale serve` + the whois CLI. The loopback
	// listener below still runs, so on-host hooks reach the daemon unchanged.
	if *tsnetEnabled {
		ts, err := serveTailnet(ctx, apiSrv, handler, *tsnetHostname, *tsnetDir)
		if err != nil {
			log.Fatalf("tsnet: %v", err)
		}
		defer ts.Close()
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
// identity backend to the node's in-process WhoIs, and serves the handler over
// HTTPS on the node's :443 (background). The auth key comes from TS_AUTHKEY (or
// prior persisted state in dir); first unauthenticated run logs a login URL.
func serveTailnet(ctx context.Context, apiSrv *api.Server, handler http.Handler, hostname, dir string) (*tsnet.Server, error) {
	ts := &tsnet.Server{Hostname: hostname, Dir: dir, UserLogf: log.Printf}
	if _, err := ts.Up(ctx); err != nil {
		return nil, fmt.Errorf("node up: %w", err)
	}
	lc, err := ts.LocalClient()
	if err != nil {
		ts.Close()
		return nil, fmt.Errorf("local client: %w", err)
	}
	apiSrv.SetIdentityResolver(tailnet.NewLocalResolver(lc))
	ln, err := ts.ListenTLS("tcp", ":443")
	if err != nil {
		ts.Close()
		return nil, fmt.Errorf("listen tls (enable HTTPS certs in the tailnet admin console): %w", err)
	}
	ip4, _ := ts.TailscaleIPs()
	log.Printf("tsnet node up: %s (ip %s), https on the tailnet, cert domains %v", hostname, ip4, ts.CertDomains())
	go func() {
		if err := http.Serve(ln, handler); err != nil && err != http.ErrServerClosed {
			log.Printf("tsnet serve stopped: %v", err)
		}
	}()
	return ts, nil
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
