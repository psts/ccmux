package llmserve

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"net"
	"net/http"
	"time"

	"ccmux.dev/ccmuxd/internal/harness"
	"ccmux.dev/ccmuxd/internal/llmproxy"
)

// Config is what the service is started with.
type Config struct {
	// DB is the daemon's database file, read only here.
	DB string
	// Addr is the loopback address the service listens on.
	Addr string
	// Unit is the mark of the unit file the service was started from (-unit,
	// written into ExecStart by the daemon). The service only repeats it on
	// GET /health; "" when it was started by hand.
	Unit string
}

// drain is how long a stop waits for requests in flight. A stop only comes
// with a change to this service's own code, and whatever is still streaming
// when the wait ends is cut; new connections are refused meanwhile, which a
// client retries. Long enough for a short request to finish, short enough
// that nobody waits on a refused port for long.
const drain = 10 * time.Second

// NewService is the proxy over the daemon's database: llmproxy's own service,
// reading through r, with panes resolved to their harness from the same file.
func NewService(r *Reader) *llmproxy.Service {
	svc := llmproxy.New(r)
	svc.SetPaneHarness(HarnessRules(r.paneHarness, harness.New(r)))
	return svc
}

// Handler is everything the service answers: the pane routes panes point
// their ANTHROPIC_BASE_URL at, its health, and the reads the daemon asks it
// for (control.go). All of it loopback only, for the reason the daemon's own
// mount gives (api/server.go): a keyed account injects a stored credential,
// and panes are local.
func Handler(svc *llmproxy.Service, cfg Config) http.Handler {
	mux := http.NewServeMux()
	llm := svc.Handler()
	// Per-METHOD patterns, as on the daemon; GET also covers HEAD (claude
	// probes HEAD /api/hello on startup).
	for _, m := range []string{"GET", "POST", "PUT", "DELETE"} {
		mux.Handle(m+" /llm/pane/{pane}/{rest...}", llm)
	}
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, Health{OK: true, CodeHash: CodeHash(), DB: cfg.DB, Unit: cfg.Unit})
	})
	control{svc}.mount(mux)
	return loopbackOnly(mux)
}

// Health is GET /health: enough for the daemon to tell whether what answers
// on the port is its own service, and whether it runs the code the daemon
// was built with.
type Health struct {
	OK bool `json:"ok"`
	// CodeHash names the service's code (hash.go). The daemon restarts the
	// service when this differs from its own.
	CodeHash string `json:"codeHash"`
	// DB is the database this service reads: a daemon with another one is
	// looking at somebody else's service.
	DB string `json:"db"`
	// Unit is the mark of the unit file this process was started from. The
	// daemon restarts the service when it is not the mark of the unit file
	// it would write now: what is on disk says what systemd will start
	// next, only the running process can say what it was started as.
	Unit string `json:"unit"`
}

func loopbackOnly(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		host, _, err := net.SplitHostPort(r.RemoteAddr)
		if ip := net.ParseIP(host); err != nil || ip == nil || !ip.IsLoopback() {
			http.Error(w, "loopback-only endpoint", http.StatusForbidden)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// Serve runs the service until ctx ends, then drains.
func Serve(ctx context.Context, cfg Config) error {
	host, _, err := net.SplitHostPort(cfg.Addr)
	if err != nil {
		return err
	}
	if !llmproxy.LoopbackHost(host) {
		return errors.New("the llm proxy listens on this machine only: -addr must be a loopback address, got " + cfg.Addr)
	}
	r, err := OpenReader(cfg.DB)
	if err != nil {
		return err
	}
	defer r.Close()
	ln, err := net.Listen("tcp", cfg.Addr)
	if err != nil {
		return err
	}
	log.Printf("llm proxy listening on http://%s (db %s, code %s)", cfg.Addr, cfg.DB, CodeHash())
	return serveOn(ctx, ln, Handler(NewService(r), cfg), drain)
}

// serveOn answers on ln until ctx ends, then gives requests in flight wait
// to finish before it returns.
func serveOn(ctx context.Context, ln net.Listener, h http.Handler, wait time.Duration) error {
	srv := &http.Server{Handler: h}
	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		<-ctx.Done()
		shut, cancel := context.WithTimeout(context.Background(), wait)
		defer cancel()
		if err := srv.Shutdown(shut); err != nil {
			log.Printf("llm proxy: stopped with requests still in flight after %s: %v", wait, err)
		}
	}()
	if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	<-stopped
	return nil
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		log.Printf("llm proxy: answer not written: %v", err)
	}
}
