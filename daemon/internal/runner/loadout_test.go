package runner

import (
	"errors"
	"os"
	"strings"
	"testing"

	"ccmux.dev/ccmuxd/internal/store"
)

// A session is started with the loadout its group and repo come to: the
// file is on disk before the start, the spec names it, and the row keeps
// what it says.
func TestASessionStartsWithItsLoadout(t *testing.T) {
	r := newRig(t)
	list := `{"skills":[]}`
	r.sup.cfg.Loadout = func(s store.RunnerSession) ([]byte, string, error) {
		return []byte(list), `{"signature":"` + s.Cwd + `"}`, nil
	}
	sess, err := r.sup.Create(NewSession{Cwd: "/r/app"})
	if err != nil {
		t.Fatal(err)
	}
	if sess.Loadout != `{"signature":"/r/app"}` {
		t.Fatalf("the session Create answers with says what it loaded: %q", sess.Loadout)
	}
	_, args := argsOf(t, r, sess.ID)
	if !strings.Contains(args, "--loadout "+r.paths.LoadoutFile(sess.ID)) {
		t.Fatalf("the spec names the loadout file: %q", args)
	}
	if body, err := os.ReadFile(r.paths.LoadoutFile(sess.ID)); err != nil || string(body) != list {
		t.Fatalf("the loadout is on disk before the start: %q %v", body, err)
	}
	if row, _ := r.st.RunnerSession(sess.ID); row.Loadout != `{"signature":"/r/app"}` {
		t.Fatalf("the row keeps what it started with: %q", row.Loadout)
	}
	// A restart reads the choices again: the file and the row follow.
	list = `{"skills":["qa:check"]}`
	if err := r.sup.Restart(sess.ID); err != nil {
		t.Fatal(err)
	}
	if body, _ := os.ReadFile(r.paths.LoadoutFile(sess.ID)); string(body) != list {
		t.Fatalf("a restart writes the loadout fresh: %q", body)
	}
}

func TestASessionWithNoLoadoutConfiguredStartsAsBefore(t *testing.T) {
	r := newRig(t)
	sess, _ := r.sup.Create(NewSession{Cwd: "/r/app"})
	if _, args := argsOf(t, r, sess.ID); strings.Contains(args, "--loadout") {
		t.Fatalf("no loadout, no flag: %q", args)
	}
	if _, err := os.Stat(r.paths.LoadoutFile(sess.ID)); !os.IsNotExist(err) {
		t.Fatalf("and no file: %v", err)
	}
}

// A loadout that cannot be worked out fails the start, said in the note,
// and the unit is never started on a guess.
func TestALoadoutThatFailsStopsTheStart(t *testing.T) {
	r := newRig(t)
	r.sup.cfg.Loadout = func(store.RunnerSession) ([]byte, string, error) {
		return nil, "", errors.New("the catalog does not parse")
	}
	sess, err := r.sup.Create(NewSession{Cwd: "/r/app"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(sess.Note, "the catalog does not parse") {
		t.Fatalf("the note says why: %q", sess.Note)
	}
	if r.units.active[r.paths.Unit(sess.ID)] {
		t.Fatal("the unit was started without its loadout")
	}
	if _, err := os.Stat(r.paths.SpecFile(sess.ID)); !os.IsNotExist(err) {
		t.Fatalf("and no spec was written: %v", err)
	}
}

func TestWriteLoadout_EmptyIsAFileNilIsNone(t *testing.T) {
	path := t.TempDir() + "/s.loadout.json"
	if err := writeLoadout(path, []byte{}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("an empty loadout is still a file: %v", err)
	}
	if err := writeLoadout(path, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("no loadout removes the file: %v", err)
	}
}
