package devhost

import (
	"reflect"
	"testing"
)

func TestTableRoute(t *testing.T) {
	tbl := NewTable(map[string]int{
		"chartlabs-app.dev.sanlabs.io": 3001,
		"chartlabs-api.dev.sanlabs.io": 3002,
	})

	cases := []struct {
		name string
		host string
		port int
		ok   bool
	}{
		{"exact", "chartlabs-app.dev.sanlabs.io", 3001, true},
		{"second entry", "chartlabs-api.dev.sanlabs.io", 3002, true},
		{"case folded", "ChartLabs-APP.Dev.SanLabs.IO", 3001, true},
		{"port stripped", "chartlabs-app.dev.sanlabs.io:443", 3001, true},
		{"trailing dot", "chartlabs-app.dev.sanlabs.io.", 3001, true},
		{"port and trailing dot", "chartlabs-app.dev.sanlabs.io.:443", 3001, true},
		{"unknown host", "nope.dev.sanlabs.io", 0, false},
		{"suffix only", "dev.sanlabs.io", 0, false},
		{"empty", "", 0, false},
		{"ipv6 literal", "[::1]:443", 0, false},
		{"bare label", "chartlabs-app", 0, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			port, ok := tbl.Route(c.host)
			if port != c.port || ok != c.ok {
				t.Fatalf("Route(%q) = (%d, %v), want (%d, %v)", c.host, port, ok, c.port, c.ok)
			}
		})
	}
}

func TestTableHostsSorted(t *testing.T) {
	tbl := NewTable(map[string]int{
		"b.dev.sanlabs.io": 2,
		"a.dev.sanlabs.io": 1,
		"c.dev.sanlabs.io": 3,
	})
	want := []string{"a.dev.sanlabs.io", "b.dev.sanlabs.io", "c.dev.sanlabs.io"}
	if got := tbl.Hosts(); !reflect.DeepEqual(got, want) {
		t.Fatalf("Hosts() = %v, want %v", got, want)
	}
}

func TestTableNormalizesKeys(t *testing.T) {
	// Keys are normalized at construction so a mixed-case mapping still routes.
	tbl := NewTable(map[string]int{"App.Dev.SanLabs.IO": 3001})
	if port, ok := tbl.Route("app.dev.sanlabs.io"); !ok || port != 3001 {
		t.Fatalf("Route after mixed-case construction = (%d, %v), want (3001, true)", port, ok)
	}
}

func TestEmptyTable(t *testing.T) {
	tbl := NewTable(nil)
	if _, ok := tbl.Route("anything.dev.sanlabs.io"); ok {
		t.Fatal("empty table routed a host")
	}
	if hosts := tbl.Hosts(); len(hosts) != 0 {
		t.Fatalf("empty table Hosts() = %v, want empty", hosts)
	}
}
