package main

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"ccmux.dev/ccmuxd/internal/manager"
	"ccmux.dev/ccmuxd/internal/store"
	"ccmux.dev/ccmuxd/internal/tmux"
)

func TestPathBetweenMarksIgnoresWhatAProfilePrints(t *testing.T) {
	for in, want := range map[string]string{
		loginPathMark + "/usr/local/go/bin:/usr/bin" + loginPathMark:          "/usr/local/go/bin:/usr/bin",
		"Welcome back!\n" + loginPathMark + "/a:/b" + loginPathMark + "\nbye": "/a:/b",
		"no marks at all":             "",
		loginPathMark + "/a:/b":       "", // cut off before the second mark
		loginPathMark + loginPathMark: "",
		"":                            "",
	} {
		if got := pathBetweenMarks(in); got != want {
			t.Errorf("pathBetweenMarks(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestReadLoginPathAsksALoginShell(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not installed")
	}
	path := readLoginPath(30 * time.Second)
	if path == "" || strings.Contains(path, loginPathMark) || strings.Contains(path, "\n") {
		t.Fatalf("the login shell's PATH = %q", path)
	}
}

func TestWithLocalBinPutsItFirstOnlyWhenMissing(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skip("no home")
	}
	local := filepath.Join(home, ".local", "bin")
	if got := withLocalBin("/usr/bin:/bin"); got != local+":/usr/bin:/bin" {
		t.Fatalf("missing: %q", got)
	}
	has := "/usr/bin:" + local + ":/bin"
	if got := withLocalBin(has); got != has {
		t.Fatalf("there already, order kept: %q", got)
	}
}

// Gap 10: a builder loads the person's whole Claude setup, as it did in a
// pane; every other session keeps what its group and repo chose.
func TestABuilderHasNoLoadout(t *testing.T) {
	asked := 0
	choose := sessionLoadout(func(store.RunnerSession) ([]byte, string, error) {
		asked++
		return []byte(`{}`), `{"signature":"x"}`, nil
	})
	if file, record, err := choose(store.RunnerSession{Role: store.RoleBuilder}); file != nil || record != "" || err != nil || asked != 0 {
		t.Fatalf("a builder's loadout = %q %q %v (asked %d), want none", file, record, err, asked)
	}
	if file, _, err := choose(store.RunnerSession{Role: store.RoleHelper}); file == nil || err != nil || asked != 1 {
		t.Fatalf("a helper keeps its loadout: %q %v", file, err)
	}
	failing := sessionLoadout(func(store.RunnerSession) ([]byte, string, error) { return nil, "", errors.New("catalog unreadable") })
	if _, _, err := failing(store.RunnerSession{}); err == nil {
		t.Fatal("a loadout that fails still fails the start of a session that has one")
	}
}

// Gap 10: only a builder gets the login shell's PATH; a person's session
// keeps the daemon's, as before.
func TestOnlyABuilderGetsTheLoginPath(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not installed")
	}
	st, err := store.Open(filepath.Join(t.TempDir(), "reg.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	mgr := manager.New(context.Background(), &tmux.Server{Socket: "unused"}, st)
	plain := sessionEnv(mgr, store.RunnerSession{PaneID: "p1"})
	if _, has := plain["PATH"]; has || plain["CCMUX_PANE_ID"] != "p1" {
		t.Fatalf("a person's session's own variables = %v", plain)
	}
	b := sessionEnv(mgr, store.RunnerSession{PaneID: "p2", Role: store.RoleBuilder})
	if b["CCMUX_PANE_ID"] != "p2" || b["PATH"] == "" || !strings.Contains(b["PATH"], filepath.Join(".local", "bin")) {
		t.Fatalf("a builder's own variables = %v", b)
	}
}
