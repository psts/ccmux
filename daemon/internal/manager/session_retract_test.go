package manager

import (
	"testing"

	"ccmux.dev/ccmuxd/internal/model"
)

// sessionSignals records what the manager reported to the peers bus.
type sessionSignals struct {
	sigs []model.SessionSignal
}

func (s *sessionSignals) sink(_, _ string, sig model.SessionSignal) {
	s.sigs = append(s.sigs, sig)
}

func (s *sessionSignals) last() model.SessionSignal {
	if len(s.sigs) == 0 {
		return ""
	}
	return s.sigs[len(s.sigs)-1]
}

// The shell backstop tells the bus a pane holds no session, and it fires on
// every command signal — restarts included, because tmux replays each pane's
// current command on subscribe. Nothing retracted it, so on a host with no
// Claude hooks installed one badly-timed restart hid a working session for the
// life of the pane. Claude in the foreground is the same class of evidence and
// has to be able to withdraw it.
func TestApplyPaneTitleSignal_ClaudeRetractsTheShellVerdict(t *testing.T) {
	m, _ := devhostManager(t)
	rec := &sessionSignals{}
	m.SessionSink = rec.sink
	p := &model.Pane{ID: "pane-1", WorkspaceID: "w1"}
	m.mu.Lock()
	m.byID["w1"].ws.Panes = append(m.byID["w1"].ws.Panes, p)
	m.mu.Unlock()

	m.applyPaneTitleSignal("w1", "pane-1", "pane-command", "zsh")
	if got := rec.last(); got != model.SessionNone {
		t.Fatalf("a bare shell reported %q, want %q", got, model.SessionNone)
	}

	// BOTH spellings tmux reports for the same program: the bare version
	// (Claude Code renames its process) and a plain "claude", which is what the
	// Linux host shows — and the one that used to be unrecognized.
	for _, cmd := range []string{"claude", "2.1.211"} {
		m.applyPaneTitleSignal("w1", "pane-1", "pane-command", "zsh")
		m.applyPaneTitleSignal("w1", "pane-1", "pane-command", cmd)
		if got := rec.last(); got != model.SessionUnknown {
			t.Errorf("foreground %q reported %q, want %q — the shell verdict stands",
				cmd, got, model.SessionUnknown)
		}
	}
}

// A running Claude repaints its title constantly. The retraction carries no new
// evidence there, and firing on it re-asserted the withdrawal on every repaint.
func TestApplyPaneTitleSignal_TitleRepaintDoesNotRetract(t *testing.T) {
	m, _ := devhostManager(t)
	rec := &sessionSignals{}
	m.SessionSink = rec.sink
	p := &model.Pane{ID: "pane-1", WorkspaceID: "w1"}
	m.mu.Lock()
	m.byID["w1"].ws.Panes = append(m.byID["w1"].ws.Panes, p)
	m.mu.Unlock()

	m.applyPaneTitleSignal("w1", "pane-1", "pane-command", "claude")
	before := len(rec.sigs)
	m.applyPaneTitleSignal("w1", "pane-1", "pane-title", "✳ Claude Code")

	for _, sig := range rec.sigs[before:] {
		if sig == model.SessionUnknown {
			t.Fatal("a title repaint re-asserted the retraction")
		}
	}
}

// The retraction must not fire for anything else running in a pane: a dev
// server or an editor is not evidence that a Claude session is there, and
// clearing the record for it would resurrect a pane the backstop correctly
// retired.
func TestApplyPaneTitleSignal_OtherCommandsDoNotRetract(t *testing.T) {
	m, _ := devhostManager(t)
	rec := &sessionSignals{}
	m.SessionSink = rec.sink
	p := &model.Pane{ID: "pane-1", WorkspaceID: "w1"}
	m.mu.Lock()
	m.byID["w1"].ws.Panes = append(m.byID["w1"].ws.Panes, p)
	m.mu.Unlock()

	m.applyPaneTitleSignal("w1", "pane-1", "pane-command", "zsh")
	before := len(rec.sigs)
	m.applyPaneTitleSignal("w1", "pane-1", "pane-command", "node")

	for _, sig := range rec.sigs[before:] {
		if sig == model.SessionUnknown {
			t.Fatal("a non-Claude command retracted the shell verdict")
		}
	}
}
