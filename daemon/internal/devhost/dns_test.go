package devhost

import (
	"context"
	"errors"
	"net/netip"
	"sync/atomic"
	"testing"
	"time"

	"github.com/libdns/libdns"
)

// fakeDNS accepts writes only for its one zone, like a scoped Cloudflare token.
type fakeDNS struct {
	zone string
	got  map[string][]libdns.Record // zone → last SetRecords payload
}

func (f *fakeDNS) SetRecords(_ context.Context, zone string, recs []libdns.Record) ([]libdns.Record, error) {
	if f.got == nil {
		f.got = map[string][]libdns.Record{}
	}
	if zone != f.zone {
		return nil, errors.New("zone not found")
	}
	f.got[zone] = recs
	return recs, nil
}

func TestUpsertWildcard_ZoneWalk(t *testing.T) {
	ip := netip.MustParseAddr("100.80.17.39")

	// The registrable zone is one suffix up: the record name stays relative
	// ("*.dev" in zone "sanlabs.io").
	f := &fakeDNS{zone: "sanlabs.io."}
	if err := upsertWildcard(context.Background(), f, "dev.sanlabs.io", ip); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	recs := f.got["sanlabs.io."]
	if len(recs) != 1 {
		t.Fatalf("records = %v", recs)
	}
	addr := recs[0].(libdns.Address)
	if addr.Name != "*.dev" || addr.IP != ip {
		t.Fatalf("record = %+v, want *.dev → %s", addr, ip)
	}

	// A zone hosted at the domain itself uses the bare wildcard.
	f = &fakeDNS{zone: "dev.sanlabs.io."}
	if err := upsertWildcard(context.Background(), f, "dev.sanlabs.io", ip); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	if addr := f.got["dev.sanlabs.io."][0].(libdns.Address); addr.Name != "*" {
		t.Fatalf("record name = %q, want *", addr.Name)
	}

	// No writable zone anywhere (and the bare TLD is never attempted) → error.
	f = &fakeDNS{zone: "io."}
	if err := upsertWildcard(context.Background(), f, "dev.sanlabs.io", ip); err == nil {
		t.Fatal("expected error when only the TLD zone would match")
	}
}

// chanDNS reports every write so a test can assert on what reached the provider
// (and, just as importantly, that nothing did).
type chanDNS struct{ writes chan libdns.Record }

func (c *chanDNS) SetRecords(_ context.Context, _ string, recs []libdns.Record) ([]libdns.Record, error) {
	for _, r := range recs {
		c.writes <- r
	}
	return recs, nil
}

// dnsServer wires a server to a recording provider. ensureDNSLocked is called
// directly rather than through Refresh: Refresh with a non-empty token would arm
// real ACME issuance against Let's Encrypt.
func dnsServer(t *testing.T, owns *atomic.Bool) (*Server, chan libdns.Record) {
	t.Helper()
	s := NewServer(context.Background(), &fakeState{}, t.TempDir(), "tailtest.ts.net",
		netip.MustParseAddr("100.80.201.127"), owns.Load)
	writes := make(chan libdns.Record, 4)
	s.newDNS = func(string) dnsProvider { return &chanDNS{writes} }
	return s, writes
}

func wroteRecord(t *testing.T, writes chan libdns.Record) libdns.Record {
	t.Helper()
	select {
	case r := <-writes:
		return r
	case <-time.After(2 * time.Second):
		t.Fatal("expected a DNS write, got none")
		return nil
	}
}

func wroteNothing(t *testing.T, writes chan libdns.Record) {
	t.Helper()
	select {
	case r := <-writes:
		t.Fatalf("expected no DNS write, got %+v", r)
	case <-time.After(200 * time.Millisecond):
	}
}

// TestEnsureDNS_OnlyOwnerWrites pins the multi-host rule: a member daemon leaves
// the shared wildcard record alone, and takes it over only if it becomes the
// owner. Without this, the last daemon to start pointed the whole dev domain at
// its own routing table, which knows none of the other hosts' hostnames.
func TestEnsureDNS_OnlyOwnerWrites(t *testing.T) {
	owns := &atomic.Bool{}
	s, writes := dnsServer(t, owns)

	s.mu.Lock()
	s.ensureDNSLocked("dev.sanlabs.io", "tok")
	s.mu.Unlock()
	wroteNothing(t, writes)

	owns.Store(true)
	s.mu.Lock()
	s.ensureDNSLocked("dev.sanlabs.io", "tok")
	s.mu.Unlock()
	// chanDNS accepts any zone, so the walk stops at the domain itself and the
	// record name is the bare wildcard (TestUpsertWildcard_ZoneWalk covers the
	// scoped-token case where it becomes "*.dev").
	addr, ok := wroteRecord(t, writes).(libdns.Address)
	if !ok || addr.Name != "*" || addr.IP.String() != "100.80.201.127" {
		t.Fatalf("record = %+v, want * → 100.80.201.127", addr)
	}
}

// TestEnsureDNS_RegainingOwnershipRewrites pins the other half of the rule: a
// daemon that owned the record, lost it to a hub joining, then owns it again
// must WRITE, not skip. Nothing in (domain, token, ip) changed across that
// round trip, so the freshness key alone would call the record already correct
// while it actually points at the hub that has since left.
func TestEnsureDNS_RegainingOwnershipRewrites(t *testing.T) {
	owns := &atomic.Bool{}
	owns.Store(true)
	s, writes := dnsServer(t, owns)

	ensure := func() {
		s.mu.Lock()
		defer s.mu.Unlock()
		s.ensureDNSLocked("dev.sanlabs.io", "tok")
	}
	ensure()
	wroteRecord(t, writes)

	owns.Store(false) // a hub joined the tailnet
	ensure()
	wroteNothing(t, writes)

	owns.Store(true) // and left again
	ensure()
	wroteRecord(t, writes)
}

// TestEnsureDNS_ReassertsWhenStale pins the self-heal: the owner rewrites the
// record on an interval, so a value some other writer stomped comes back without
// a daemon restart.
func TestEnsureDNS_ReassertsWhenStale(t *testing.T) {
	owns := &atomic.Bool{}
	owns.Store(true)
	s, writes := dnsServer(t, owns)

	s.mu.Lock()
	s.ensureDNSLocked("dev.sanlabs.io", "tok")
	s.mu.Unlock()
	wroteRecord(t, writes)

	// Nothing changed and the assert is fresh: no second write.
	s.mu.Lock()
	s.ensureDNSLocked("dev.sanlabs.io", "tok")
	s.mu.Unlock()
	wroteNothing(t, writes)

	s.mu.Lock()
	s.dnsAt = s.dnsAt.Add(-DNSReassertInterval - time.Second)
	s.ensureDNSLocked("dev.sanlabs.io", "tok")
	s.mu.Unlock()
	wroteRecord(t, writes)
}

// TestStartDNSHeal_RefreshesOnItsInterval pins the loop itself: with nothing
// changing, the reconcile keeps running, which is what gives ensureDNSLocked
// the chance to notice a stale assert and rewrite the record. Without the loop
// a record something else overwrote stays wrong until the daemon restarts.
//
// It asserts on reconciles rather than on DNS writes: a write needs a Cloudflare
// token, and a token in a Refresh test would arm real ACME issuance in
// ensureCertLocked. The write itself is pinned by TestEnsureDNS_ReassertsWhenStale.
func TestStartDNSHeal_RefreshesOnItsInterval(t *testing.T) {
	owns := &atomic.Bool{}
	owns.Store(true)
	st := &fakeState{}
	s := NewServer(context.Background(), st, t.TempDir(), "tailtest.ts.net",
		netip.MustParseAddr("100.80.201.127"), owns.Load)

	s.StartDNSHeal(20 * time.Millisecond)
	deadline := time.Now().Add(2 * time.Second)
	for st.reconciles.Load() < 3 {
		if time.Now().After(deadline) {
			t.Fatalf("reconciles = %d after 2s, want the heal loop to keep running", st.reconciles.Load())
		}
		time.Sleep(5 * time.Millisecond)
	}
}
