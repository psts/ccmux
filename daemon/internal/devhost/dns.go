package devhost

import (
	"context"
	"fmt"
	"log"
	"net/netip"
	"strings"
	"time"

	"github.com/libdns/libdns"
)

// A-record self-heal: in custom-domain mode the daemon owns the one DNS record
// the feature needs — `*.<devDomain>` A <its own tailnet IP> — through the same
// Cloudflare token that issues certs. Public DNS holding a CGNAT 100.x address
// is deliberate: anyone can resolve it, only tailnet devices can connect, and
// the record survives node re-registration because every Refresh re-asserts it.

// dnsProvider is the libdns slice cloudflare.Provider satisfies; a seam so
// tests never talk to Cloudflare.
type dnsProvider interface {
	SetRecords(ctx context.Context, zone string, recs []libdns.Record) ([]libdns.Record, error)
}

// ensureDNSLocked re-asserts the wildcard A record when (domain, token, ip)
// changed since the last successful assert. Runs in the background — Refresh
// must not block on the Cloudflare API.
func (s *Server) ensureDNSLocked(domain, token string) {
	if token == "" || !s.selfIP.IsValid() {
		return
	}
	key := domain + "\x00" + token + "\x00" + s.selfIP.String()
	if s.dnsKey == key {
		return
	}
	s.dnsKey = key // optimistic; cleared on failure so the next Refresh retries
	provider := s.newDNS(token)
	go func() {
		if err := upsertWildcard(s.ctx, provider, domain, s.selfIP); err != nil {
			log.Printf("devhost: dns self-heal *.%s: %v", domain, err)
			s.mu.Lock()
			if s.dnsKey == key {
				s.dnsKey = ""
			}
			s.mu.Unlock()
			return
		}
		log.Printf("devhost: dns ok: *.%s A %s", domain, s.selfIP)
	}()
}

// upsertWildcard sets `*.<domain>` A ip, discovering the Cloudflare zone by
// walking domain suffixes ("dev.sanlabs.io" tries itself, then "sanlabs.io").
func upsertWildcard(ctx context.Context, p dnsProvider, domain string, ip netip.Addr) error {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	var lastErr error
	labels := strings.Split(domain, ".")
	for i := 0; i < len(labels)-1; i++ { // never try the bare TLD
		zone := strings.Join(labels[i:], ".")
		name := "*"
		if i > 0 {
			name = "*." + strings.Join(labels[:i], ".")
		}
		_, err := p.SetRecords(ctx, zone+".", []libdns.Record{
			libdns.Address{Name: name, TTL: 5 * time.Minute, IP: ip},
		})
		if err == nil {
			return nil
		}
		lastErr = err
	}
	return fmt.Errorf("no writable zone found for %s: %w", domain, lastErr)
}
