package runner

// The failure table, live: real systemd units, the real sidecar, the host's
// claude, a scratch registry. Off unless CCMUX_RUNNER_LIVE=1 (it spends a
// few cents of Haiku and takes minutes), and it needs:
//   CCMUX_RUNNER_EXEC     a ccmuxd binary built from this tree (runner-exec)
//   CCMUX_RUNNER_MODULES  a node_modules holding the pinned SDK
// It writes its own ccmux-session-live@ template into ~/.config/systemd/user
// and removes it afterwards, so a daemon's ccmux-session@ is never touched.

import (
	"bufio"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"ccmux.dev/ccmuxd/internal/store"
	"ccmux.dev/ccmuxd/internal/walls"
)

type live struct {
	t     *testing.T
	st    *store.SQLite
	paths Paths
	cfg   Config
	sup   *Supervisor
}

func newLive(t *testing.T) *live {
	if os.Getenv("CCMUX_RUNNER_LIVE") != "1" {
		t.Skip("set CCMUX_RUNNER_LIVE=1 to run against real systemd and claude")
	}
	root, err := os.MkdirTemp("", "rl")
	if err != nil {
		t.Fatal(err)
	}
	l := &live{t: t, paths: Paths{State: filepath.Join(root, "st"), Runtime: filepath.Join(root, "s"), Prefix: "ccmux-session-live"}}
	l.st, err = store.Open(filepath.Join(root, "reg.db"))
	if err != nil {
		t.Fatal(err)
	}
	script := installSidecar(t, filepath.Join(root, "sc"))
	node, _ := exec.LookPath("node")
	claude, _ := exec.LookPath("claude")
	l.cfg = Config{Sidecar: func() (string, string, error) { return script, node, nil }, Claude: claude, Env: os.Environ}
	home, _ := os.UserHomeDir()
	unitDir := filepath.Join(home, ".config", "systemd", "user")
	if err := EnsureTemplate(unitDir, os.Getenv("CCMUX_RUNNER_EXEC"), l.paths, walls.Defaults().Caps(), Systemctl{}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		os.Remove(filepath.Join(unitDir, l.paths.Template()))
		Systemctl{}.Reload()
		l.st.Close()
		os.RemoveAll(root)
	})
	l.sup = New(l.st, Systemctl{}, l.paths, l.cfg)
	return l
}

// installSidecar copies the sidecar's shipped modules next to the pinned
// SDK, as EnsureClaudeServe would.
func installSidecar(t *testing.T, dir string) string {
	src, _ := filepath.Abs("../agent/claudeserve")
	os.MkdirAll(dir, 0o755)
	files, _ := filepath.Glob(filepath.Join(src, "*.mjs"))
	for _, f := range append(files, filepath.Join(src, "package.json")) {
		if strings.HasSuffix(f, ".test.mjs") {
			continue
		}
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		os.WriteFile(filepath.Join(dir, filepath.Base(f)), b, 0o644)
	}
	if err := os.Symlink(os.Getenv("CCMUX_RUNNER_MODULES"), filepath.Join(dir, "node_modules")); err != nil {
		t.Fatal(err)
	}
	return filepath.Join(dir, "serve.mjs")
}

// until reconciles every two seconds until cond holds.
func (l *live) until(t *testing.T, what string, limit time.Duration, cond func() bool) {
	t.Helper()
	end := time.Now().Add(limit)
	for time.Now().Before(end) {
		l.sup.Reconcile(context.Background())
		if cond() {
			return
		}
		time.Sleep(2 * time.Second)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// transcript is the session's JSONL lines, from Claude Code's own store.
func transcript(id string) []map[string]any {
	home, _ := os.UserHomeDir()
	matches, _ := filepath.Glob(filepath.Join(home, ".claude", "projects", "*", id+".jsonl"))
	var out []map[string]any
	for _, m := range matches {
		f, err := os.Open(m)
		if err != nil {
			continue
		}
		sc := bufio.NewScanner(f)
		sc.Buffer(make([]byte, 1<<20), 16<<20)
		for sc.Scan() {
			var line map[string]any
			if json.Unmarshal(sc.Bytes(), &line) == nil {
				out = append(out, line)
			}
		}
		f.Close()
	}
	return out
}

// said counts transcript lines of a type whose JSON holds text.
func said(id, typ, text string) int {
	n := 0
	for _, line := range transcript(id) {
		b, _ := json.Marshal(line)
		if line["type"] == typ && strings.Contains(string(b), text) {
			n++
		}
	}
	return n
}

func withUUID(id, uuid string) int {
	n := 0
	for _, line := range transcript(id) {
		if line["type"] == "user" && line["uuid"] == uuid {
			n++
		}
	}
	return n
}

func mainPID(unit string) string {
	out, _ := exec.Command("systemctl", "--user", "show", "-p", "MainPID", "--value", unit).Output()
	return strings.TrimSpace(string(out))
}

// idle waits for the session to finish what it is doing, so one row's
// leftover turn (a resumed count) does not run into the next row.
func (l *live) idle(t *testing.T, sessionID string) {
	t.Helper()
	l.until(t, "the session to go idle", 5*time.Minute, func() bool {
		h, err := Dial(l.paths.Socket(sessionID)).Hello(context.Background())
		return err == nil && !h.Busy
	})
}

func (l *live) delivered(sessionID string) bool {
	open, _ := l.st.UndeliveredPrompts(sessionID)
	return len(open) == 0
}

func TestLiveFailureTable(t *testing.T) {
	l := newLive(t)
	work := t.TempDir()
	sess, err := l.sup.Create(NewSession{Cwd: work, Name: "live", Model: "haiku"})
	if err != nil {
		t.Fatal(err)
	}
	unit := l.paths.Unit(sess.ID)
	t.Cleanup(func() { l.sup.SetWanted(sess.ID, store.SessionDone) })

	t.Run("a prompt is answered", func(t *testing.T) {
		l.sup.Send(sess.ID, "Reply with exactly: PONG-1", "test")
		l.until(t, "PONG-1", 3*time.Minute, func() bool { return said(sess.ID, "assistant", "PONG-1") > 0 })
	})

	t.Run("a daemon restart mid-turn leaves the turn running", func(t *testing.T) {
		pid := mainPID(unit)
		l.sup.Send(sess.ID, "Without using any tools, count from 1 to 150, one number per line, then write END-2.", "test")
		l.until(t, "the count to start", 2*time.Minute, func() bool { return l.delivered(sess.ID) })
		l.sup = New(l.st, Systemctl{}, l.paths, l.cfg) // the daemon came back
		l.until(t, "END-2", 3*time.Minute, func() bool { return said(sess.ID, "assistant", "END-2") > 0 })
		if mainPID(unit) != pid {
			t.Fatalf("the runner was not restarted by a daemon restart: pid %s -> %s", pid, mainPID(unit))
		}
	})

	t.Run("a runner killed mid-turn comes back, resumes and is told to check", func(t *testing.T) {
		pid := mainPID(unit)
		l.sup.Send(sess.ID, "Without using any tools, write out every number from 1 to 800 in words, one per line, then write END-3.", "test")
		l.until(t, "the long count to start", 2*time.Minute, func() bool { return l.delivered(sess.ID) })
		h, err := Dial(l.paths.Socket(sess.ID)).Hello(context.Background())
		if err != nil || !h.TurnOpen {
			t.Fatalf("the turn should be open at the kill: %+v %v", h, err)
		}
		exec.Command("systemctl", "--user", "kill", "-s", "KILL", unit).Run()
		l.until(t, "systemd to restart it", time.Minute, func() bool { p := mainPID(unit); return p != pid && p != "0" })
		l.until(t, "the interruption notice", 3*time.Minute, func() bool { return said(sess.ID, "user", "[ccmux] This session was restarted") == 1 })
		if said(sess.ID, "assistant", "PONG-1") == 0 {
			t.Fatal("the resumed session lost its history")
		}
	})

	t.Run("a crash during a prompt queued behind a turn still gets the notice", func(t *testing.T) {
		l.idle(t, sess.ID)
		pid := mainPID(unit)
		l.sup.Send(sess.ID, "Reply with exactly: FIRST-5", "test")
		l.sup.Send(sess.ID, "Without using any tools, write out every number from 1 to 800 in words, one per line, then write END-5.", "test")
		l.until(t, "both prompts in the transcript", 2*time.Minute, func() bool { return l.delivered(sess.ID) })
		l.until(t, "the second turn running", 2*time.Minute, func() bool { return said(sess.ID, "assistant", "FIRST-5") > 0 })
		h, err := Dial(l.paths.Socket(sess.ID)).Hello(context.Background())
		if err != nil || !h.TurnOpen {
			t.Fatalf("the second prompt keeps the turn open: %+v %v", h, err)
		}
		exec.Command("systemctl", "--user", "kill", "-s", "KILL", unit).Run()
		l.until(t, "systemd to restart it", time.Minute, func() bool { p := mainPID(unit); return p != pid && p != "0" })
		l.until(t, "the notice naming the second prompt", 3*time.Minute, func() bool {
			return said(sess.ID, "user", "[ccmux] This session was restarted") == 2 && said(sess.ID, "user", "1 to 800 in words, one per line, then write END-5.") >= 2
		})
	})

	t.Run("a prompt sent while everything is down arrives once after the start", func(t *testing.T) {
		l.idle(t, sess.ID)
		exec.Command("systemctl", "--user", "stop", unit).Run()         // as after a reboot
		exec.Command("systemctl", "--user", "reset-failed", unit).Run() // a reboot forgets the start count too
		id, _ := l.sup.Send(sess.ID, "Reply with exactly: PONG-4", "test")
		l.sup = New(l.st, Systemctl{}, l.paths, l.cfg) // the daemon's first start after the reboot
		l.until(t, "PONG-4", 3*time.Minute, func() bool { return said(sess.ID, "assistant", "PONG-4") > 0 })
		for i := 0; i < 3; i++ {
			l.sup.Reconcile(context.Background())
		}
		if n := withUUID(sess.ID, id); n != 1 {
			t.Fatalf("the prompt is in the transcript %d times, want once", n)
		}
	})

	exec.Command("systemctl", "--user", "reset-failed", unit).Run() // keep the kills above off the start limit
	t.Run("a card dies with its runner and is marked expired", func(t *testing.T) {
		l.idle(t, sess.ID)
		l.sup.Send(sess.ID, "Use the AskUserQuestion tool to ask me whether to pick red or blue. Do nothing else.", "test")
		var card string
		l.until(t, "the question card", 3*time.Minute, func() bool {
			open, _ := l.st.OpenSessionCards(sess.ID)
			if len(open) > 0 {
				card = open[0].ID
			}
			return card != ""
		})
		pid := mainPID(unit)
		exec.Command("systemctl", "--user", "kill", "-s", "KILL", unit).Run()
		l.until(t, "the card to expire", 2*time.Minute, func() bool {
			p := mainPID(unit)
			if p == pid || p == "0" {
				return false
			}
			open, _ := l.st.OpenSessionCards(sess.ID)
			for _, c := range open {
				if c.ID == card {
					return false
				}
			}
			return true
		})
	})

	t.Run("a done session is stopped", func(t *testing.T) {
		if err := l.sup.SetWanted(sess.ID, store.SessionDone); err != nil {
			t.Fatal(err)
		}
		if active, _ := (Systemctl{}).Active(unit); active {
			t.Fatal("still running")
		}
	})

	// Its own scratch daemon and its own unit template (live_bus_test.go).
	t.Run("a bus message reaches a session as a prompt, wakes it asleep, and its reply comes back", liveBusRow)
	// The thread page's stream, through the same kind of scratch daemon
	// (live_stream_test.go).
	t.Run("a follower of the session's stream has every event once and in order through a daemon restart and a runner kill", liveStreamRow)
}
