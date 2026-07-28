package manager

import (
	"testing"

	"ccmux.dev/ccmuxd/internal/model"
)

func TestStartsClaude(t *testing.T) {
	cases := map[string]bool{
		"claude":                      true,
		"claude --resume":             true,
		"/usr/local/bin/claude -c":    true,
		"FOO=bar claude":              true,
		"claude --dangerously-load x": true,
		"":                            false,
		"zsh":                         false,
		"npm run dev":                 false,
		"python3 manage.py runserver": false,
		"claudette":                   false, // near-miss must not match
		"echo claude":                 false, // claude as an argument, not the program
	}
	for cmd, want := range cases {
		if got := startsClaude(cmd); got != want {
			t.Errorf("startsClaude(%q) = %v, want %v", cmd, got, want)
		}
	}
}

// A pane that has hosted Claude and is now at a bare shell is dormant — the
// case where a dead teammate is indistinguishable from a working one. The rule
// reads HostedClaude, NOT the startup command, so a Claude the user launched by
// hand counts exactly the same.
func TestIsDormant(t *testing.T) {
	claudePane := func(rawCmd string) *model.Pane {
		return &model.Pane{HostedClaude: true, RawCommand: rawCmd}
	}
	if !isDormant(claudePane("zsh")) {
		t.Error("a claude pane sitting at a shell must be dormant")
	}
	if isDormant(claudePane("2.1.220")) {
		t.Error("a pane running claude must not be dormant")
	}
	// Real work the user started after Claude exited is NOT dormant — labelling
	// it would be worse than saying nothing.
	if isDormant(claudePane("vim")) {
		t.Error("a pane running another program must not be dormant")
	}
	if isDormant(claudePane("node")) {
		t.Error("a pane running node must not be dormant")
	}
	// A pane that has never hosted Claude has no session to lose.
	if isDormant(&model.Pane{RawCommand: "zsh"}) {
		t.Error("a plain terminal pane must never be dormant")
	}
	// The dev-server pane has its own lifecycle and its own UI affordance.
	if isDormant(&model.Pane{HostedClaude: true, RawCommand: "zsh", DevServer: true}) {
		t.Error("the dev pane is not a dormant claude")
	}
	// The whole point of the change: a hand-launched Claude carries no startup
	// command, and must still go dormant once it exits.
	handLaunched := &model.Pane{HostedClaude: true, StartupCommand: "", RawCommand: "zsh"}
	if !isDormant(handLaunched) {
		t.Error("a hand-launched Claude that exited must be dormant")
	}
}

// atBareShell is the backstop observation, and it must recognise a shell and
// nothing else — saying "no session" about a pane running real work would hide
// a live teammate.
func TestAtBareShell(t *testing.T) {
	for _, cmd := range []string{"zsh", "bash", "-zsh", "fish", "sh"} {
		if !atBareShell(&model.Pane{RawCommand: cmd}) {
			t.Errorf("%q is a bare shell", cmd)
		}
	}
	for _, cmd := range []string{"2.1.220", "vim", "node", "Python", "", "npm"} {
		if atBareShell(&model.Pane{RawCommand: cmd}) {
			t.Errorf("%q is not a bare shell", cmd)
		}
	}
}

// refreshDormantLocked reports only real transitions, so the caller persists
// and broadcasts once rather than on every command signal.
func TestRefreshDormantReportsTransitionsOnly(t *testing.T) {
	p := &model.Pane{HostedClaude: true, RawCommand: "2.1.220"}
	if refreshDormantLocked(p) {
		t.Fatal("a running claude pane is not a change from the zero value")
	}
	p.RawCommand = "zsh"
	if !refreshDormantLocked(p) || !p.Dormant {
		t.Fatal("claude exiting must register as a transition")
	}
	if refreshDormantLocked(p) {
		t.Fatal("no further change should be reported while it stays dormant")
	}
	p.RawCommand = "2.1.220"
	if !refreshDormantLocked(p) || p.Dormant {
		t.Fatal("a new claude starting must clear dormancy")
	}
}

// hosted_claude is what makes a hand-launched Claude visible to us at all: the
// hook environment comes from the PANE, so any positive signal proves a session
// ran there whoever started it. The bit is sticky for the life of the pane.
func TestApplySession_MarksHostedClaudeOnAnyPositive(t *testing.T) {
	for _, sig := range []model.SessionSignal{model.SessionStarted, model.SessionActive} {
		p := &model.Pane{StartupCommand: "", RawCommand: "zsh"}
		if isDormant(p) {
			t.Fatal("precondition: a pane with no known Claude is not dormant")
		}
		p.HostedClaude = sig == model.SessionStarted || sig == model.SessionActive
		p.Dormant = isDormant(p)
		if !p.Dormant {
			t.Errorf("%s should mark the pane as having hosted Claude, making it dormant at a shell", sig)
		}
	}
}
