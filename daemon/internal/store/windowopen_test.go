package store

import (
	"database/sql"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// writePreDeviceRegistry builds a window_open exactly as a daemon before the
// per-device change wrote it: PRIMARY KEY (login, window_id), no device column.
//
// Every other test opens a fresh database, where the schema already has the
// device column — so the PRAGMA guard returns early and the whole rebuild is
// dead code under test. Every existing install takes the other branch exactly
// once, which is the branch that can lose data.
func writePreDeviceRegistry(t *testing.T, path string, rows [][2]string) {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`CREATE TABLE window_open (
  login TEXT, window_id TEXT, PRIMARY KEY (login, window_id)
)`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`CREATE TABLE windows (id TEXT PRIMARY KEY, name TEXT NOT NULL)`); err != nil {
		t.Fatal(err)
	}
	for _, r := range rows {
		if _, err := db.Exec(`INSERT INTO window_open (login, window_id) VALUES (?,?)`, r[0], r[1]); err != nil {
			t.Fatal(err)
		}
	}
}

// The real upgrade. If the rebuild drops a row, or swaps the two columns in its
// INSERT ... SELECT, every existing user's windows silently change state — and
// if Open returns an error, the daemon will not start at all.
func TestWindowOpen_PreDeviceRegistryUpgrades(t *testing.T) {
	path := filepath.Join(t.TempDir(), "old.db")
	writePreDeviceRegistry(t, path, [][2]string{
		{"patric@x.com", "win-a"},
		{"patric@x.com", "win-b"},
		{"dasha@x.com", "win-a"},
	})

	st, err := Open(path)
	if err != nil {
		t.Fatalf("open pre-device registry: %v", err)
	}
	defer st.Close()

	opens, err := st.WindowOpens(0)
	if err != nil {
		t.Fatal(err)
	}
	// Every row survived, and login/window did not get transposed by the SELECT.
	if !opens["win-a"]["patric@x.com"] || !opens["win-a"]["dasha@x.com"] {
		t.Fatalf("win-a openers = %v, want both logins", opens["win-a"])
	}
	if !opens["win-b"]["patric@x.com"] {
		t.Fatalf("win-b openers = %v, want patric", opens["win-b"])
	}
	if len(opens) != 2 {
		t.Fatalf("windows = %v, want exactly win-a and win-b", opens)
	}
}

// Migrated rows must expire SOON. They carry device=” because nothing knows
// which lens set them, so no lens can name one to clear it; while one stands,
// WindowOpens keeps reporting that login as an opener, `last` never comes back
// true, and archive-on-last-close is silently off. Stamping them with `now`
// gave that a full 30 days.
func TestWindowOpen_MigratedRowsExpireWithinADay(t *testing.T) {
	path := filepath.Join(t.TempDir(), "old.db")
	writePreDeviceRegistry(t, path, [][2]string{{"patric@x.com", "win-a"}})

	st, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	// Still counted now, so nobody's windows change on upgrade.
	live, err := st.WindowOpens(time.Now().Add(-OpenFlagTTL).UnixMilli())
	if err != nil {
		t.Fatal(err)
	}
	if !live["win-a"]["patric@x.com"] {
		t.Fatal("the migrated row is already stale: windows would flip closed on upgrade")
	}
	// And gone a day and a bit later.
	soon := time.Now().Add(25 * time.Hour).Add(-OpenFlagTTL).UnixMilli()
	if later, err := st.WindowOpens(soon); err != nil {
		t.Fatal(err)
	} else if later["win-a"]["patric@x.com"] {
		t.Fatal("the migrated row outlives a day, so archive-on-last-close stays dead")
	}
}

// Opening twice must be a no-op the second time. The guard is the only thing
// standing between a restart and a rebuild that would collapse every real
// device id back to the empty string.
func TestWindowOpen_MigrationIsIdempotent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "old.db")
	writePreDeviceRegistry(t, path, [][2]string{{"patric@x.com", "win-a"}})

	st, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.SetWindowOpen(WindowOpenFlag{
		Login: "patric@x.com", WindowID: "win-a", Device: "mac-1", Label: "patric-mbp",
		Seen: time.Now().UnixMilli(),
	}, true); err != nil {
		t.Fatal(err)
	}
	st.Close()

	again, err := Open(path)
	if err != nil {
		t.Fatalf("reopen after migration: %v", err)
	}
	defer again.Close()

	mine, err := again.DeviceWindowOpens("patric@x.com", "mac-1", 0)
	if err != nil {
		t.Fatal(err)
	}
	if !mine["win-a"] {
		t.Fatal("a second Open rebuilt the table and wiped the real device id")
	}
}

// A close names one device, so it must not disturb any other row — that is the
// whole reason the device belongs in the key. Including a device=” row, which
// is what a lens too old to send one writes: deleting that made the close
// answer last=true and force-archive workspaces the old lens still showed.
func TestWindowOpen_CloseTouchesOnlyItsOwnDevice(t *testing.T) {
	path := filepath.Join(t.TempDir(), "old.db")
	writePreDeviceRegistry(t, path, [][2]string{{"patric@x.com", "win-a"}})

	st, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	now := time.Now().UnixMilli()
	// "" is a live lens that predates the device param, not a leftover.
	for _, dev := range []string{"", "mac-1", "web-1"} {
		if err := st.SetWindowOpen(WindowOpenFlag{
			Login: "patric@x.com", WindowID: "win-a", Device: dev, Seen: now,
		}, true); err != nil {
			t.Fatal(err)
		}
	}
	if err := st.SetWindowOpen(WindowOpenFlag{
		Login: "patric@x.com", WindowID: "win-a", Device: "web-1",
	}, false); err != nil {
		t.Fatal(err)
	}

	mac, err := st.DeviceWindowOpens("patric@x.com", "mac-1", 0)
	if err != nil {
		t.Fatal(err)
	}
	if !mac["win-a"] {
		t.Fatal("one lens's close cleared another lens's row")
	}
	web, err := st.DeviceWindowOpens("patric@x.com", "web-1", 0)
	if err != nil {
		t.Fatal(err)
	}
	if web["win-a"] {
		t.Fatal("the close did not clear its own row")
	}
	old, err := st.DeviceWindowOpens("patric@x.com", "", 0)
	if err != nil {
		t.Fatal(err)
	}
	if !old["win-a"] {
		t.Fatal("the close deleted a lens that sends no device — its live windows would be force-archived")
	}
}

// Two daemons opening the same pre-device file at once. The guard narrows the
// race but does not literally close it — a deferred transaction takes no write
// lock until its first write — so this measures the outcome rather than
// assuming it: whoever loses must ERROR, never silently rebuild the table the
// winner just migrated and collapse every real device id back to ”.
func TestWindowOpen_ConcurrentOpensDoNotClobber(t *testing.T) {
	path := filepath.Join(t.TempDir(), "race.db")
	writePreDeviceRegistry(t, path, [][2]string{{"patric@x.com", "win-a"}})

	var wg sync.WaitGroup
	opened := make([]*SQLite, 2)
	errs := make([]error, 2)
	start := make(chan struct{})
	for i := range opened {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			opened[i], errs[i] = Open(path)
		}(i)
	}
	close(start)
	wg.Wait()

	var live *SQLite
	for i, st := range opened {
		if st != nil {
			defer st.Close()
			if live == nil {
				live = st
			}
		}
		if errs[i] != nil {
			t.Logf("open %d lost the race and errored, which is the safe outcome: %v", i, errs[i])
		}
	}
	if live == nil {
		t.Fatalf("both opens failed: %v / %v", errs[0], errs[1])
	}
	// Whatever the interleaving, the pre-upgrade row must still be there and
	// the table must be usable — not rebuilt twice into an empty shape.
	opens, err := live.WindowOpens(0)
	if err != nil {
		t.Fatal(err)
	}
	if !opens["win-a"]["patric@x.com"] {
		t.Fatalf("the row did not survive concurrent migration: %v", opens)
	}
	// And a real device id written afterwards must stick.
	if err := live.SetWindowOpen(WindowOpenFlag{
		Login: "patric@x.com", WindowID: "win-a", Device: "mac-1", Seen: time.Now().UnixMilli(),
	}, true); err != nil {
		t.Fatal(err)
	}
	mine, err := live.DeviceWindowOpens("patric@x.com", "mac-1", 0)
	if err != nil {
		t.Fatal(err)
	}
	if !mine["win-a"] {
		t.Fatal("a real device id did not survive")
	}
}

// Re-asserting an existing flag must move last_seen. Both keep-alives (the
// Mac's 12-hourly re-send and the browser's) go through this one clause; if
// it silently becomes INSERT OR IGNORE, a lens in continuous use ages out of
// its own flags, and the next close elsewhere force-archives what it shows.
func TestWindowOpen_ReassertMovesLastSeen(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "fresh.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	now := time.Now().UnixMilli()
	flag := WindowOpenFlag{Login: "patric@x.com", WindowID: "win-a", Device: "mac-1"}
	flag.Seen = now - 2*time.Hour.Milliseconds()
	if err := st.SetWindowOpen(flag, true); err != nil {
		t.Fatal(err)
	}
	cutoff := now - time.Hour.Milliseconds()
	before, err := st.DeviceWindowOpens("patric@x.com", "mac-1", cutoff)
	if err != nil {
		t.Fatal(err)
	}
	if before["win-a"] {
		t.Fatal("the first row is not stale yet, so the assertion below cannot tell whether the re-assert moved it")
	}
	flag.Seen = now
	if err := st.SetWindowOpen(flag, true); err != nil {
		t.Fatal(err)
	}
	after, err := st.DeviceWindowOpens("patric@x.com", "mac-1", cutoff)
	if err != nil {
		t.Fatal(err)
	}
	if !after["win-a"] {
		t.Fatal("re-asserting an open flag did not move last_seen: keep-alives are no-ops")
	}
}
