// Command ccmuxd is the ccmux daemon: it owns a dedicated tmux server that holds
// persistent Claude Code sessions and serves a REST + WebSocket API that lenses
// (native app, web, phone) attach to. v1 binds localhost only; Tailscale
// identity lands in a later phase.
package main

import (
	"context"
	"flag"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	"ccmux.dev/ccmuxd/config"
	"ccmux.dev/ccmuxd/internal/api"
	"ccmux.dev/ccmuxd/internal/manager"
	"ccmux.dev/ccmuxd/internal/store"
	"ccmux.dev/ccmuxd/internal/tmux"
)

func main() {
	socket := flag.String("socket", "ccmux", "tmux server socket name (-L)")
	addr := flag.String("addr", "127.0.0.1:7890", "HTTP listen address")
	dbPath := flag.String("db", defaultDBPath(), "registry SQLite path")
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
	if err := mgr.Start(); err != nil {
		log.Fatalf("manager start: %v", err)
	}

	httpSrv := &http.Server{Addr: *addr, Handler: api.NewServer(mgr).Handler()}
	go func() {
		<-ctx.Done()
		shutCtx, c := context.WithTimeout(context.Background(), 3e9)
		defer c()
		_ = httpSrv.Shutdown(shutCtx)
	}()

	log.Printf("ccmuxd listening on http://%s (tmux -L %s, db %s)", *addr, *socket, *dbPath)
	if err := httpSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatalf("http: %v", err)
	}
	log.Print("ccmuxd stopped")
}

// defaultDBPath returns ~/Library/Application Support/ccmuxd/ccmuxd.db (or the
// XDG/OS equivalent on the future Linux host).
func defaultDBPath() string {
	dir, err := os.UserConfigDir()
	if err != nil {
		dir = os.TempDir()
	}
	return filepath.Join(dir, "ccmuxd", "ccmuxd.db")
}
