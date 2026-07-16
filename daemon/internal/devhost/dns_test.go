package devhost

import (
	"context"
	"errors"
	"net/netip"
	"testing"

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
