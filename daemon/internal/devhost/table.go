// Package devhost serves hosted workspaces' dev servers at stable HTTPS names
// over the tailnet ("dev hostnames"). A Table snapshot maps FQDNs to local
// ports; the serving side (custom-domain SNI dispatch or per-hostname tsnet
// nodes) consults it per request. Tables are immutable — mutations build a new
// one — so request-path lookups never take a lock.
package devhost

import (
	"net"
	"sort"
	"strings"
)

// Table is an immutable hostname→port snapshot.
type Table struct {
	byHost map[string]int
}

// NewTable builds a Table from FQDN→port. Keys are normalized (lowercase,
// no trailing dot) so construction-time casing can't break request routing.
func NewTable(byHost map[string]int) *Table {
	m := make(map[string]int, len(byHost))
	for host, port := range byHost {
		m[normalizeHost(host)] = port
	}
	return &Table{byHost: m}
}

// Route resolves an HTTP Host header (or TLS SNI) to a local port. It accepts
// the forms browsers actually send: optional :port, any casing, trailing dot.
func (t *Table) Route(host string) (port int, ok bool) {
	port, ok = t.byHost[normalizeHost(host)]
	return port, ok
}

// Hosts returns the routed FQDNs, sorted, for diagnostics (the unknown-host page).
func (t *Table) Hosts() []string {
	hosts := make([]string, 0, len(t.byHost))
	for h := range t.byHost {
		hosts = append(hosts, h)
	}
	sort.Strings(hosts)
	return hosts
}

// normalizeHost lowercases, drops a :port suffix, and trims the FQDN root dot.
func normalizeHost(host string) string {
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	return strings.ToLower(strings.TrimSuffix(host, "."))
}
