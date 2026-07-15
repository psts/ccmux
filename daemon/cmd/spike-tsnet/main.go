// Command spike-tsnet is the S5 gate for the tsnet migration: it brings the
// daemon up as its OWN tailnet node (separate from the host's tailscaled),
// serves an HTTPS endpoint on that node's :443 with a tailnet-issued cert, and
// resolves the caller's identity in-process via LocalClient().WhoIs — proving
// two nodes on one Mac + per-connection WhoIs before touching the real daemon.
//
// First run logs an auth URL (unless TS_AUTHKEY is set); click it once to
// authorize the `ccmuxd-spike` node, after which state in Dir persists.
package main

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"

	"tailscale.com/tsnet"
)

func main() {
	base, err := os.UserConfigDir()
	if err != nil {
		base = os.TempDir()
	}
	s := &tsnet.Server{
		Hostname: "ccmuxd-spike",
		Dir:      filepath.Join(base, "ccmuxd", "tsnet-spike"),
		UserLogf: log.Printf, // surfaces the auth URL + status
	}
	defer s.Close()

	if _, err := s.Up(context.Background()); err != nil {
		log.Fatalf("tsnet up: %v", err)
	}
	ip4, _ := s.TailscaleIPs()
	log.Printf("node up: ip=%s certDomains=%v", ip4, s.CertDomains())

	lc, err := s.LocalClient()
	if err != nil {
		log.Fatalf("local client: %v", err)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/whoami", func(w http.ResponseWriter, r *http.Request) {
		who, err := lc.WhoIs(r.Context(), r.RemoteAddr)
		if err != nil {
			fmt.Fprintf(w, "whois error for %s: %v\n", r.RemoteAddr, err)
			return
		}
		fmt.Fprintf(w, "login=%s display=%q node=%s\n",
			who.UserProfile.LoginName, who.UserProfile.DisplayName, who.Node.ComputedName)
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprintln(w, "ccmuxd tsnet spike ok")
	})

	ln, err := s.ListenTLS("tcp", ":443")
	if err != nil {
		log.Fatalf("listen TLS: %v", err)
	}
	log.Printf("serving https on the tailnet node — try https://ccmuxd-spike.<tailnet>.ts.net/whoami")
	log.Fatal(http.Serve(ln, mux))
}
