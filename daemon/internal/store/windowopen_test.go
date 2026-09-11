package store

import (
	"database/sql"
	"path/filepath"
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

// A close names one device, so it must not disturb another's row — that shared
// row is the whole reason the device belongs in the key. It DOES clear the
// login's pre-upgrade row, which no lens can name and which otherwise blocks
// archive-on-last-close until it ages out.
func TestWindowOpen_CloseIsScopedToItsDeviceAndSweepsLegacy(t *testing.T) {
	path := filepath.Join(t.TempDir(), "old.db")
	writePreDeviceRegistry(t, path, [][2]string{{"patric@x.com", "win-a"}})

	st, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	now := time.Now().UnixMilli()
	for _, dev := range []string{"mac-1", "web-1"} {
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
	legacy, err := st.DeviceWindowOpens("patric@x.com", "", 0)
	if err != nil {
		t.Fatal(err)
	}
	if legacy["win-a"] {
		t.Fatal("the pre-upgrade row survived a close, so `last` can never come back true")
	}
}
