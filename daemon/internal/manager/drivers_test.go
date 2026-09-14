package manager

import (
	"context"
	"path/filepath"
	"testing"

	"ccmux.dev/ccmuxd/internal/store"
)

// The remembered driver survives the manager that recorded it: a second
// manager over the same store answers the same login. That is the restart
// case. "anon" is never recorded, and a repeat keystroke by the same person
// is not a store write (asserted by the timestamp moving without a change).
func TestRecordDriver_PersistsAcrossManagers(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "reg.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	m := New(context.Background(), nil, st)
	m.RecordDriver("w1", "anon", 1)
	if _, ok := m.LastDriver("w1"); ok {
		t.Fatal("anon must not be remembered as a driver")
	}
	m.RecordDriver("w1", "dasha@x.com", 10)
	m.RecordDriver("w1", "dasha@x.com", 20)
	if d, _ := m.LastDriver("w1"); d.At != 20 {
		t.Fatalf("in-memory At = %d, want the latest keystroke", d.At)
	}
	m.RecordDriver("w2", "dasha@x.com", 30)
	m.RecordDriver("w3", "dasha@x.com", 40)

	again := New(context.Background(), nil, st)
	d, ok := again.LastDriver("w1")
	if !ok || d.Login != "dasha@x.com" {
		t.Fatalf("driver after a fresh manager = %+v ok=%v, want dasha", d, ok)
	}
	if d.At != 10 {
		t.Fatalf("repeat keystroke by the same driver rewrote the store (at=%d, want the tenure start 10)", d.At)
	}

	// An unidentified typist displaces the memory, persistently; a removed
	// workspace is forgotten, persistently.
	again.RecordDriver("w2", "anon", 50)
	again.ForgetDriver("w3")
	third := New(context.Background(), nil, st)
	if all := third.LastDrivers(); len(all) != 1 || all["w1"].Login != "dasha@x.com" {
		t.Fatalf("LastDrivers after displace+forget = %+v, want only w1", all)
	}
}

// No store: memory only, never a panic. The presence tests build managers
// this way.
func TestRecordDriver_StorelessIsMemoryOnly(t *testing.T) {
	m := New(context.Background(), nil, nil)
	m.RecordDriver("w1", "patric@x.com", 5)
	if d, ok := m.LastDriver("w1"); !ok || d.Login != "patric@x.com" {
		t.Fatalf("driver = %+v ok=%v", d, ok)
	}
}
