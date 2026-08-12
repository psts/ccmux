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

// DNSReassertInterval is how often the owner rewrites the record even when
// nothing changed (what production passes to StartDNSHeal). The record has
// exactly one correct value and one writer, so re-asserting is how it heals
// from anything that overwrote it — another daemon that used to claim it, a
// hand edit in the Cloudflare dashboard — without waiting for a restart.
const DNSReassertInterval = 5 * time.Minute

// ensureDNSLocked re-asserts the wildcard A record when (domain, token, ip)
// changed since the last successful assert, or when that assert has gone stale.
// Runs in the background — Refresh must not block on the Cloudflare API.
//
// Only the fleet's DNS owner writes. Every daemon holding the dev domain and a
// token used to assert this record at its OWN tailnet IP, so in a multi-host
// fleet the last daemon to start silently took the whole domain with it: its
// routing table has none of the other hosts' hostnames, so every dev URL served
// "no workspace maps ..." while the mappings sat intact on the hub. The hub is
// the only correct target — it reverse-proxies a member's hostname to that
// member (see api.Server.WrapDevhost) — so a member with a hub on the tailnet
// must never write the record.
func (s *Server) ensureDNSLocked(domain, token string) {
	// Cheap local disqualifiers first: ownsDNS reads the tailnet status, and a
	// daemon with nothing to write should not pay for that on every reconcile.
	if token == "" || !s.selfIP.IsValid() {
		return
	}
	if !s.ownsDNS() {
		if !s.dnsMuted {
			log.Printf("devhost: not writing *.%s — this daemon does not own that record", domain)
			s.dnsMuted = true
		}
		s.dnsKey = "" // regaining ownership must re-assert, not skip on a stale key
		return
	}
	s.dnsMuted = false
	every := s.dnsEvery
	if every == 0 {
		every = DNSReassertInterval
	}
	key := domain + "\x00" + token + "\x00" + s.selfIP.String()
	if s.dnsKey == key && time.Since(s.dnsAt) < every {
		return
	}
	// Optimistic; cleared on failure so the next Refresh retries.
	s.dnsKey, s.dnsAt = key, time.Now()
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
