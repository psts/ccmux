package model

import "testing"

// TestUnmarshalHostnames_MigratesAllocatedPorts: rows the daemon allocated
// from its old reserved range load as the port the app binds itself.
func TestUnmarshalHostnames_MigratesAllocatedPorts(t *testing.T) {
	raw := `[{"name":"chartlabs","port":21000,"targetPort":3003},` +
		`{"name":"chartlabs-thor","port":21001,"targetPort":8000},` +
		`{"name":"kullio-admin","port":8082},` +
		`{"name":"odd","port":21005}]`
	got := UnmarshalHostnames(raw)
	want := []Hostname{{Name: "chartlabs", Port: 3003}, {Name: "chartlabs-thor", Port: 8000}, {Name: "kullio-admin", Port: 8082}, {Name: "odd", Port: 21005}}
	if len(got) != len(want) {
		t.Fatalf("got %+v", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("row %d = %+v, want %+v", i, got[i], want[i])
		}
	}
	// Re-marshaling writes the migrated form: targetPort is gone for good.
	if s := MarshalHostnames(got); s != `[{"name":"chartlabs","port":3003},{"name":"chartlabs-thor","port":8000},{"name":"kullio-admin","port":8082},{"name":"odd","port":21005}]` {
		t.Fatalf("marshal = %s", s)
	}
	if UnmarshalHostnames("") != nil || UnmarshalHostnames("not json") != nil {
		t.Fatal("empty and malformed blobs must load as no mappings")
	}
}

func TestHostname_RoutePort(t *testing.T) {
	if (Hostname{Port: 5173}).RoutePort() != 5173 || (Hostname{Port: 5173, LivePort: 5174}).RoutePort() != 5174 {
		t.Fatal("RoutePort must prefer LivePort")
	}
	if (Hostname{Port: 3003, HeldBy: "other"}).RoutePort() != 0 {
		t.Fatal("a held name must route nowhere")
	}
	if !IsLegacyAllocatedPort(21000) || !IsLegacyAllocatedPort(21999) || IsLegacyAllocatedPort(20999) || IsLegacyAllocatedPort(3003) {
		t.Fatal("IsLegacyAllocatedPort bounds")
	}
}
