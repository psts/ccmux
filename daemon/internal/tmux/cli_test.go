package tmux

import "testing"

// TestServer_RunRefusesShellFormat covers the transport formatCommand does not.
//
// The #( refusal originally lived only in formatCommand, which guards control
// mode. Server.NewSession goes out through exec.Command instead, and
// new-session expands its -c value exactly as new-window does — so every
// workspace create, and every revive replaying a stored pane0.CWD, stayed
// exploitable. Confirmed on tmux 3.4 before this: a cwd of
// "<dir>/#(id>/tmp/x)" created the session and ran the command.
//
// cwd reaches here straight off the wire: POST /v1/workspaces carries it and
// the only prior check is that it is non-empty.
func TestServer_RunRefusesShellFormat(t *testing.T) {
	// Its own socket, torn down both ways: if the guard ever regresses, this
	// test really does create a session, and leaving it behind would make the
	// NEXT run fail on the leftover rather than on the bug.
	s := &Server{Socket: "ccmux-cliguard-itest"}
	_ = s.KillServer()
	t.Cleanup(func() { _ = s.KillServer() })

	if err := s.NewSession("victim", "/repos/#(touch /tmp/pwned)", 80, 24, nil); err == nil {
		t.Fatal("NewSession accepted a cwd containing #( — that runs /bin/sh " +
			"when tmux expands -c")
	}
	// Nothing may have been executed: a refused command must not reach tmux.
	if s.HasSession("victim") {
		t.Error("a refused NewSession still reached tmux and created a session")
	}

	// Env values travel the same argv, and are refused for the same reason.
	if err := s.NewSession("victim", "/tmp", 80, 24,
		map[string]string{"X": "#(touch /tmp/pwned)"}); err == nil {
		t.Error("NewSession accepted an env value containing #(")
	}

	// The everyday arguments must keep working — this check runs on every CLI
	// command the daemon issues, so an over-broad refusal would break them all.
	for _, ok := range [][]string{
		{"has-session", "-t", "=ccmux-app-72eb9fa9"},
		{"list-sessions", "-F", "#{session_name}"},
		{"new-session", "-d", "-s", "x", "-c", "/home/me/My Projects/app"},
		{"kill-session", "-t", "=x"},
	} {
		if err := rejectShellFormatAll(ok); err != nil {
			t.Errorf("refused a legitimate command %q: %v", ok, err)
		}
	}
}

// rejectShellFormatAll mirrors what run does, so the allowlist above is checked
// against the same rule without spawning tmux.
func rejectShellFormatAll(args []string) error {
	for _, a := range args {
		if err := rejectShellFormat(a); err != nil {
			return err
		}
	}
	return nil
}
