package manager

import (
	"testing"

	"ccmux.dev/ccmuxd/internal/model"
)

// Pins the automatic pane-title policy (lens tab labels). Signals verified live
// on tmux 3.6b: #{pane_current_command} is "zsh" for idle shells, the running
// program otherwise (Claude Code's process name is its bare version, e.g.
// "2.1.211"); #{pane_title} is the OSC title ("✳ Claude Code" for claude,
// the host's name — e.g. "MBP.local" — when nothing set one).
func TestDerivePaneTitle(t *testing.T) {
	defaults := map[string]bool{"MBP.local": true, "MBP": true}
	cases := []struct {
		name, title, cmd, want string
	}{
		{"no signal yet leaves existing", "", "", ""},
		{"idle shell is Terminal", "MBP.local", "zsh", "Terminal"},
		{"login shell dash prefix", "MBP.local", "-zsh", "Terminal"},
		{"stale claude title loses to a live shell", "✳ Claude Code", "zsh", "Terminal"},
		{"claude title normalizes", "✳ Claude Code", "2.1.211", "Claude"},
		{"claude before its title arrives (version argv0)", "MBP.local", "2.1.211", "Claude"},
		{"program with default title keeps command name", "MBP.local", "node", "node"},
		{"program title wins over command", "user@box: ~/src", "ssh", "user@box: ~/src"},
		{"vim without title", "", "vim", "vim"},
	}
	for _, c := range cases {
		if got := derivePaneTitle(c.title, c.cmd, defaults); got != c.want {
			t.Errorf("%s: derivePaneTitle(%q, %q) = %q, want %q", c.name, c.title, c.cmd, got, c.want)
		}
	}
}

func TestInitialPaneTitle(t *testing.T) {
	if got := initialPaneTitle("claude --continue"); got != "Claude" {
		t.Errorf("claude startup = %q, want Claude", got)
	}
	if got := initialPaneTitle(""); got != "Terminal" {
		t.Errorf("plain shell = %q, want Terminal", got)
	}
	if got := initialPaneTitle("npm run dev"); got != "Terminal" {
		t.Errorf("other command = %q, want Terminal (runtime signals refine it)", got)
	}
}

// TestApplyPaneTitleSignal pins the runtime flow: tmux signals fold into the
// pane, a real change persists + republishes, and the dev-server pane's
// purposeful "dev ▸ …" title is never overwritten.
func TestApplyPaneTitleSignal(t *testing.T) {
	m, st := devhostManager(t)
	p := &model.Pane{ID: "pane-1", WorkspaceID: "w1"}
	dev := &model.Pane{ID: "pane-2", WorkspaceID: "w1", DevServer: true, Title: "dev ▸ npm run dev"}
	m.mu.Lock()
	m.byID["w1"].ws.Panes = append(m.byID["w1"].ws.Panes, p, dev)
	m.mu.Unlock()

	m.applyPaneTitleSignal("w1", "pane-1", "pane-command", "zsh")
	if p.Title != "Terminal" {
		t.Fatalf("after zsh: title = %q, want Terminal", p.Title)
	}
	m.applyPaneTitleSignal("w1", "pane-1", "pane-command", "2.1.211")
	m.applyPaneTitleSignal("w1", "pane-1", "pane-title", "✳ Claude Code")
	if p.Title != "Claude" {
		t.Fatalf("after claude signals: title = %q, want Claude", p.Title)
	}

	// applyPaneTitleSignal persisted the change itself.
	loaded, err := st.Load()
	if err != nil {
		t.Fatal(err)
	}
	for _, l := range loaded {
		if l.ID != "w1" {
			continue
		}
		for _, lp := range l.Panes {
			if lp.ID == "pane-1" && lp.Title != "Claude" {
				t.Fatalf("persisted title = %q, want Claude", lp.Title)
			}
		}
	}

	// The dev-server pane keeps its purposeful title.
	m.applyPaneTitleSignal("w1", "pane-2", "pane-command", "node")
	if dev.Title != "dev ▸ npm run dev" {
		t.Fatalf("dev pane title = %q, want unchanged", dev.Title)
	}

	// Unknown pane / workspace: no panic, no effect.
	m.applyPaneTitleSignal("w1", "nope", "pane-command", "zsh")
	m.applyPaneTitleSignal("nope", "pane-1", "pane-command", "zsh")
}
