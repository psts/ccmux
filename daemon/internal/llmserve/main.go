package llmserve

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"
)

// Main is `ccmuxd llm-proxy`: the unit's ExecStart, and with -code-hash the
// one-line answer the deploy script compares against the running service.
// It lives here rather than in cmd/ccmuxd so that the whole of the service,
// its flags included, is inside the hashed code.
func Main(args []string) error {
	fs := flag.NewFlagSet("llm-proxy", flag.ContinueOnError)
	db := fs.String("db", "", "the daemon's database file (read only)")
	addr := fs.String("addr", "", "loopback address to listen on, e.g. 127.0.0.1:7901")
	unit := fs.String("unit", "", "the mark of the unit file this service is started from; repeated on GET /health for the daemon to compare")
	hash := fs.Bool("code-hash", false, "print the hash of this binary's proxy code and exit")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *hash {
		fmt.Println(CodeHash())
		return nil
	}
	if *db == "" || *addr == "" {
		return errors.New("usage: ccmuxd llm-proxy -db PATH -addr 127.0.0.1:PORT")
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	return Serve(ctx, Config{DB: *db, Addr: *addr, Unit: *unit})
}
