package store

import (
	"database/sql"
	"fmt"
	"testing"
)

func sidebarStore(t *testing.T, path string) *SQLite {
	t.Helper()
	s, err := Open(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	return s
}

// prefsLine renders one login's prefs as "id:collapsed:position" in the
// order served, "-" for no position, so a test compares one string.
func prefsLine(t *testing.T, s *SQLite, login string) string {
	t.Helper()
	list, err := s.SidebarPrefs(login)
	if err != nil {
		t.Fatalf("prefs of %s: %v", login, err)
	}
	out := ""
	for _, p := range list {
		pos := "-"
		if p.Position != nil {
			pos = fmt.Sprint(*p.Position)
		}
		out += fmt.Sprintf("%s:%v:%s ", p.WindowID, p.Collapsed, pos)
	}
	return out
}

// A fold and an order survive a reopen; each keeps the other; a new order
// clears the old one's places; unknown windows land nowhere; two logins
// never see each other's choices; deleting a window takes every login's
// row for it.
func TestSidebarPrefsRoundTrip(t *testing.T) {
	path := t.TempDir() + "/reg.db"
	s := sidebarStore(t, path)
	for _, w := range [][2]string{{"a", "ALPHA"}, {"b", "BETA"}, {"c", "GAMMA"}} {
		if err := s.CreateWindow(w[0], w[1]); err != nil {
			t.Fatal(err)
		}
	}
	if ok, err := s.SetSidebarCollapsed("patric", "c", true); err != nil || !ok {
		t.Fatalf("collapse = %v, %v", ok, err)
	}
	if ok, err := s.SetSidebarCollapsed("patric", "gone", true); err != nil || ok {
		t.Fatalf("collapse of an unknown window = %v, %v; want nothing stored", ok, err)
	}
	if err := s.SetSidebarOrder("patric", []string{"c", "gone", "a", "b"}); err != nil {
		t.Fatal(err)
	}
	if err := s.SetSidebarOrder("patric", []string{"b", "c"}); err != nil {
		t.Fatal(err)
	}
	if err := s.SetSidebarOrder("dasha", []string{"a"}); err != nil {
		t.Fatal(err)
	}
	s.Close()

	s = sidebarStore(t, path)
	defer s.Close()
	if got, want := prefsLine(t, s, "patric"), "b:false:0 c:true:1 a:false:- "; got != want {
		t.Fatalf("patric = %q, want %q", got, want)
	}
	if got, want := prefsLine(t, s, "dasha"), "a:false:0 "; got != want {
		t.Fatalf("dasha = %q, want %q (patric's choices leaked)", got, want)
	}
	if _, err := s.SetSidebarCollapsed("patric", "c", false); err != nil {
		t.Fatal(err)
	}
	if got, want := prefsLine(t, s, "patric"), "b:false:0 c:false:1 a:false:- "; got != want {
		t.Fatalf("unfolded = %q, want %q (the place must stay)", got, want)
	}
	if err := s.DeleteWindow("b"); err != nil {
		t.Fatal(err)
	}
	if got, want := prefsLine(t, s, "patric"), "c:false:1 a:false:- "; got != want {
		t.Fatalf("after delete = %q, want %q", got, want)
	}
	if got := prefsLine(t, s, "nobody"); got != "" {
		t.Fatalf("a login with no choices = %q, want none", got)
	}
}

// A database made before sidebar prefs existed gains the table on start.
func TestSidebarPrefsOnAnOldDatabase(t *testing.T) {
	path := t.TempDir() + "/old.db"
	s := sidebarStore(t, path)
	if err := s.CreateWindow("a", "ALPHA"); err != nil {
		t.Fatal(err)
	}
	s.Close()
	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := raw.Exec(`DROP TABLE sidebar_prefs`); err != nil {
		t.Fatalf("drop: %v", err)
	}
	raw.Close()

	s = sidebarStore(t, path)
	defer s.Close()
	if got := prefsLine(t, s, "patric"); got != "" {
		t.Fatalf("prefs on an old database = %q, want none", got)
	}
	if ok, err := s.SetSidebarCollapsed("patric", "a", true); err != nil || !ok {
		t.Fatalf("collapse on an old database = %v, %v", ok, err)
	}
}
