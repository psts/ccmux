// Package tailnet resolves the Tailscale identity behind a connection, so
// presence shows a verified tailnet user (LoginName / DisplayName) instead of a
// self-declared name.
//
// It shells out to `tailscale whois --json` once per distinct client IP and
// caches the result. This runs at connection time (a rare event), never on the
// keystroke path, so it honors the no-process-spawning-in-hot-paths rule. On the
// future Linux host this can be swapped for tailscale.com's in-process
// LocalClient without changing callers.
package tailnet

import (
	"encoding/json"
	"net"
	"os/exec"
	"sync"
)

// Resolver maps a connection's remote address to a Tailscale identity, caching
// by IP. The whois command is injectable for tests.
type Resolver struct {
	mu    sync.Mutex
	cache map[string]ident
	run   func(addr string) ([]byte, error)
}

type ident struct {
	login   string
	display string
	ok      bool
}

// NewResolver returns a resolver backed by the tailscale CLI.
func NewResolver() *Resolver {
	return &Resolver{cache: map[string]ident{}, run: whoisCmd}
}

func whoisCmd(addr string) ([]byte, error) {
	return exec.Command("tailscale", "whois", "--json", addr).Output()
}

// Resolve returns the verified tailnet login and display name for a connection's
// remote address. ok is false for loopback/LAN connections or when identity
// cannot be determined (caller then falls back to a self-declared name).
func (r *Resolver) Resolve(remoteAddr string) (login, display string, ok bool) {
	host := hostOf(remoteAddr)
	if isLocal(host) {
		return "", "", false
	}
	r.mu.Lock()
	if c, hit := r.cache[host]; hit {
		r.mu.Unlock()
		return c.login, c.display, c.ok
	}
	r.mu.Unlock()

	login, display, ok = r.lookup(host)
	r.mu.Lock()
	r.cache[host] = ident{login, display, ok}
	r.mu.Unlock()
	return login, display, ok
}

func (r *Resolver) lookup(host string) (string, string, bool) {
	out, err := r.run(host + ":0")
	if err != nil {
		return "", "", false
	}
	login, display, err := parseWhoisProfile(out)
	if err != nil || login == "" {
		return "", "", false
	}
	return login, display, true
}

// taggedDevicesLogin is the synthetic user Tailscale reports for a tagged node.
// It is a machine class, not a person, and must never pass as a verified login —
// the ccmux daemons themselves are tagged (tag:ccmux), so accepting it would let
// any daemon→daemon call be recorded as a user named "tagged-devices".
const taggedDevicesLogin = "tagged-devices"

// machineCaller is the one home of that rule, shared by both resolver backends
// so it cannot rot in one of them.
func machineCaller(tags []string, login string) bool {
	return len(tags) > 0 || login == taggedDevicesLogin
}

// parseWhoisProfile extracts the UserProfile identity from `tailscale whois
// --json` output. A tagged caller (Node.Tags set, or the synthetic
// tagged-devices profile) is a machine, never a person: it resolves to no
// login, and the caller falls through to its unverified tiers.
func parseWhoisProfile(data []byte) (login, display string, err error) {
	var v struct {
		Node struct {
			Tags []string
		}
		UserProfile struct {
			LoginName   string
			DisplayName string
		}
	}
	if err = json.Unmarshal(data, &v); err != nil {
		return "", "", err
	}
	if machineCaller(v.Node.Tags, v.UserProfile.LoginName) {
		return "", "", nil
	}
	return v.UserProfile.LoginName, v.UserProfile.DisplayName, nil
}

func hostOf(remoteAddr string) string {
	if host, _, err := net.SplitHostPort(remoteAddr); err == nil {
		return host
	}
	return remoteAddr
}

func isLocal(host string) bool {
	if host == "" || host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}
