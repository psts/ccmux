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
	// The shell is not asked here: under a loaded host it can run out of
	// its ten seconds, and what is tested is who gets the answer.
	was := loginPaths
	loginPaths = &pathCache{now: time.Now, read: func() string { return "/usr/local/go/bin:/usr/bin" }}
	defer func() { loginPaths = was }()
	b := sessionEnv(mgr, store.RunnerSession{PaneID: "p2", Role: store.RoleBuilder})
	if b["CCMUX_PANE_ID"] != "p2" || !strings.HasSuffix(b["PATH"], ":/usr/local/go/bin:/usr/bin") || !strings.Contains(b["PATH"], filepath.Join(".local", "bin")) {
		t.Fatalf("a builder's own variables = %v", b)
	}
	if again := sessionEnv(mgr, store.RunnerSession{PaneID: "p1"}); again["PATH"] != "" {
		t.Fatalf("a person's session got the login PATH: %v", again)
	}
}

// A role agent's session brings its own setup too (task #177): its base
// says what it loads, and its MCP servers and shell tools need the PATH
// its pane's login shell had. A repo's lead beside it keeps its loadout and
// the daemon's PATH.
func TestARoleAgentHasNoLoadoutAndTheLoginPath(t *testing.T) {
	asked := 0
	choose := sessionLoadout(func(store.RunnerSession) ([]byte, string, error) {
		asked++
		return []byte(`{}`), `{"signature":"x"}`, nil
	})
	if file, record, err := choose(store.RunnerSession{Kind: store.SessionAgent, Name: "radar"}); file != nil || record != "" || err != nil || asked != 0 {
		t.Fatalf("an agent's loadout = %q %q %v (asked %d), want none", file, record, err, asked)
	}
	if file, _, err := choose(store.RunnerSession{Kind: store.SessionLead}); file == nil || err != nil || asked != 1 {
		t.Fatalf("a lead keeps its loadout: %q %v", file, err)
	}
	st, err := store.Open(filepath.Join(t.TempDir(), "reg.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	mgr := manager.New(context.Background(), &tmux.Server{Socket: "unused"}, st)
	was := loginPaths
	loginPaths = &pathCache{now: time.Now, read: func() string { return "/opt/node/bin:/usr/bin" }}
	defer func() { loginPaths = was }()
	a := sessionEnv(mgr, store.RunnerSession{PaneID: "a1", Kind: store.SessionAgent})
	if a["CCMUX_PANE_ID"] != "a1" || !strings.HasSuffix(a["PATH"], ":/opt/node/bin:/usr/bin") {
		t.Fatalf("an agent's own variables = %v", a)
	}
	if lead := sessionEnv(mgr, store.RunnerSession{PaneID: "l1", Kind: store.SessionLead}); lead["PATH"] != "" {
		t.Fatalf("a lead got the login PATH: %v", lead)
	}
}

// A read that failed must not stand for the daemon's whole life: one slow
// shell at boot would leave every builder on the bare PATH until a restart.
func TestAFailedLoginPathIsAskedAgainAndAnAnswerIsKept(t *testing.T) {
	now := time.Date(2026, 11, 19, 12, 0, 0, 0, time.UTC)
	answers, asked := []string{"", "", "/usr/local/go/bin:/usr/bin", "never read"}, 0
	c := &pathCache{now: func() time.Time { return now }, read: func() string { asked++; return answers[asked-1] }}
	if got := c.get(); got != "" || asked != 1 {
		t.Fatalf("a failed read = %q after %d asks", got, asked)
	}
	if got := c.get(); got != "" || asked != 1 {
		t.Fatalf("inside the minute a failed read is not asked again: %q after %d asks", got, asked)
	}
	now = now.Add(loginPathRetry + time.Second)
	if got := c.get(); got != "" || asked != 2 {
		t.Fatalf("after the minute it is asked again: %q after %d asks", got, asked)
	}
	now = now.Add(loginPathRetry + time.Second)
	if got := c.get(); got != "/usr/local/go/bin:/usr/bin" || asked != 3 {
		t.Fatalf("the first answer = %q after %d asks", got, asked)
	}
	now = now.Add(24 * time.Hour)
	if got := c.get(); got != "/usr/local/go/bin:/usr/bin" || asked != 3 {
		t.Fatalf("an answer is kept for good: %q after %d asks", got, asked)
	}
}
