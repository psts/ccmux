package manager

import (
	"bytes"
	"log"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"ccmux.dev/ccmuxd/internal/claudever"
	"ccmux.dev/ccmuxd/internal/model"
)

// The hooks reach the restart clock through the manager's own entry points,
// not only through the clock's methods: a Stop starts it, a new prompt stops
// it, a permission prompt blocks it, and the idle reminder starts a fresh
// session's clock even when it alerts nothing.
func TestHooksDriveTheRestartClock(t *testing.T) {
	m, _ := seenManager(t)
	m.ApplySession("idle", "s1", model.SessionStarted)
	m.ApplyAttention("idle", model.AttentionDone, model.ReasonFinished)
	if k, _ := m.restart.clocks.get("idle"); k.idleSince.IsZero() || k.session != "s1" {
		t.Fatalf("after a Stop: %+v", k)
	}
	m.ApplyAttention("idle", model.AttentionIdle, model.ReasonNone)
	if k, _ := m.restart.clocks.get("idle"); !k.idleSince.IsZero() {
		t.Fatalf("a submitted prompt must stop the clock: %+v", k)
	}
	m.ApplyAttention("idle", model.AttentionNeedsInput, model.ReasonPermission)
	if k, _ := m.restart.clocks.get("idle"); !k.blocked {
		t.Fatalf("a permission prompt must block: %+v", k)
	}
	m.ApplySession("needs", "s2", model.SessionStarted)
	m.ApplyAttention("needs", model.AttentionIdle, model.ReasonNone) // looked at: claims nothing
	if m.Remind("needs") {
		t.Fatal("a pane claiming nothing alerts nothing")
	}
	if k, _ := m.restart.clocks.get("needs"); k.idleSince.IsZero() {
		t.Fatalf("the reminder must still start the clock: %+v", k)
	}
	if _, ok := m.restart.clocks.get("running"); ok {
		t.Fatal("a pane no hook spoke for has no clock")
	}
}

// A failure is forgotten once the pane is seen on the installed version, and
// kept while it is not, including while no Claude runs there at all.
func TestRestartFailureResolvesOnTheInstalledVersion(t *testing.T) {
	m, _ := seenManager(t)
	p := &model.Pane{ID: "idle"}
	cases := []struct {
		name    string
		running string
		failed  bool
	}{
		{"still behind", "1.0.0", true},
		{"no Claude running", "", true},
		{"on the installed version", "2.0.0", false},
	}
	for _, c := range cases {
		m.restart.fail("idle", "2.0.0")
		v, _ := m.restartViewFor(p, "2.0.0", claudever.Proc{Version: c.running})
		if v.failed != c.failed {
			t.Errorf("%s: failed=%v want %v", c.name, v.failed, c.failed)
		}
	}
}

// The view reads the session transcript's last write only for a pane that
// is behind.
func TestRestartViewReadsTheTranscript(t *testing.T) {
	m, _ := seenManager(t)
	m.restart.claudeDir = t.TempDir()
	id := "3e067cdf-2528-4c9d-8832-e099badf7b02"
	f := filepath.Join(m.restart.claudeDir, "projects", "-repo", id+".jsonl")
	writeFile(t, f, "{}\n")
	when := time.Now().Add(-2 * time.Hour).Truncate(time.Second)
	if err := os.Chtimes(f, when, when); err != nil {
		t.Fatal(err)
	}
	m.ApplySession("idle", id, model.SessionStarted)
	p := &model.Pane{ID: "idle"}
	if v, _ := m.restartViewFor(p, "2.0.0", claudever.Proc{Version: "1.0.0"}); !v.transcript.Equal(when) {
		t.Fatalf("behind: transcript %v want %v", v.transcript, when)
	}
	if v, _ := m.restartViewFor(p, "2.0.0", claudever.Proc{Version: "2.0.0"}); !v.transcript.IsZero() {
		t.Fatalf("up to date: transcript read anyway (%v)", v.transcript)
	}
}

// A skip reason is logged once per pane, version and reason, not every tick.
func TestRestartNoteLogsOnce(t *testing.T) {
	var buf bytes.Buffer
	log.SetOutput(&buf)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })
	var r autoRestart
	r.note("p", "2.0.0", "draft")
	r.note("p", "2.0.0", "draft")
	r.note("q", "2.0.0", "draft") // another pane is its own line
	r.note("p", "2.0.0", "capture failed")
	r.note("p", "2.1.0", "capture failed") // a newer version is news again
	if n := strings.Count(buf.String(), "waits:"); n != 4 {
		t.Fatalf("logged %d lines, want 4:\n%s", n, buf.String())
	}
}

// Which panes are ccmux's to restart is read off the pane itself: an agent
// (its own lifecycle restarts it), a dev server, or a pane no Claude was
// ever seen in is left alone and carries no mark, while a plain hosted
// Claude pane is eligible.
func TestRestartEligibilityFromThePane(t *testing.T) {
	m, _ := seenManager(t)
	proc := claudever.Proc{Version: "1.0.0", Argv: []string{"claude"}}
	cases := []struct {
		name string
		p    model.Pane
		want bool
	}{
		{"a plain hosted Claude pane", model.Pane{ID: "p", HostedClaude: true, StartupCommand: "claude"}, true},
		{"an agent pane", model.Pane{ID: "p", HostedClaude: true, StartupCommand: "claude", Agent: "radar"}, false},
		{"a dev-server pane", model.Pane{ID: "p", HostedClaude: true, StartupCommand: "claude", DevServer: true}, false},
		{"never hosted a Claude", model.Pane{ID: "p", StartupCommand: "claude"}, false},
		{"a startup command that is not Claude", model.Pane{ID: "p", HostedClaude: true, StartupCommand: "pnpm dev"}, false},
	}
	for _, c := range cases {
		v, _ := m.restartViewFor(&c.p, "2.0.0", proc)
		if v.eligible != c.want {
			t.Errorf("%s: eligible=%v want %v", c.name, v.eligible, c.want)
		}
		if !c.want && restartMark(v) != "" {
			t.Errorf("%s: marked %q, want no mark", c.name, restartMark(v))
		}
	}
}
