package tmux

import (
	"bytes"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
)

// Server manages a dedicated tmux server (one -L socket) via one-shot CLI
// invocations. These handle server/session lifecycle only — infrequent,
// user-driven operations, not hot-path pane I/O (that goes through a control
// mode Client). Keeping lifecycle on the CLI avoids a chicken-and-egg problem:
// control mode can only attach to a session that already exists.
type Server struct {
	Socket     string
	ConfigPath string
}

func (s *Server) run(args ...string) (string, error) {
	// Same shell-format refusal the control-mode path makes. This transport
	// passes argv directly, so it needs no quoting and newlines are harmless
	// here — but #( is expanded by tmux itself, so it is dangerous however the
	// argument arrives. NewSession puts a caller-supplied cwd straight into
	// -c, and new-session expands it exactly as new-window does.
	for _, a := range args {
		if err := rejectShellFormat(a); err != nil {
			return "", err
		}
	}
	full := append([]string{"-L", s.Socket}, args...)
	cmd := exec.Command("tmux", full...)
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
		return out.String(), fmt.Errorf("tmux %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(errb.String()))
	}
	return out.String(), nil
}

// EnsureStarted starts the server with the managed config if it isn't running.
// It deliberately does NOT set window-size manual: on tmux 3.6b, creating a
// session while window-size is globally manual crashes the server. That option
// is applied per-session after the session exists (see Controller.stampSession).
// exit-empty off (in the config) keeps this empty server alive.
func (s *Server) EnsureStarted() error {
	_, err := s.run("-f", s.ConfigPath, "start-server")
	return err
}

// SourceConfig re-applies the managed config to an ALREADY-RUNNING server.
// `-f` is only read at server spawn, and the tmux server outlives daemon
// restarts/upgrades by design — without this, config changes (e.g. the
// clipboard copy-pipe bindings) would not take effect until every session
// died and the server respawned.
func (s *Server) SourceConfig() error {
	_, err := s.run("source-file", s.ConfigPath)
	return err
}

// SetGlobal sets a global (-g) option at runtime.
func (s *Server) SetGlobal(name, value string) error {
	_, err := s.run("set-option", "-g", name, value)
	return err
}

// HasSession reports whether a session with the given name exists.
func (s *Server) HasSession(name string) bool {
	_, err := s.run("has-session", "-t", "="+name)
	return err == nil
}

// NewSession creates a detached session (and its first window/pane) at cwd with
// the given size and environment. The first window is pane index 0 of the
// workspace; further panes are added via the control connection (new-window).
func (s *Server) NewSession(name, cwd string, cols, rows int, env map[string]string) error {
	args := []string{"new-session", "-d", "-s", name,
		"-x", strconv.Itoa(cols), "-y", strconv.Itoa(rows)}
	if cwd != "" {
		args = append(args, "-c", cwd)
	}
	for k, v := range env {
		args = append(args, "-e", k+"="+v)
	}
	_, err := s.run(args...)
	return err
}

// KillSession destroys a session (and everything in it).
func (s *Server) KillSession(name string) error {
	_, err := s.run("kill-session", "-t", "="+name)
	return err
}

// KillServer tears down the whole tmux server.
func (s *Server) KillServer() error {
	_, err := s.run("kill-server")
	return err
}

// SessionMeta is the registry-relevant metadata for one tmux session.
type SessionMeta struct {
	Name        string
	Managed     bool
	WorkspaceID string
	Windows     int
}

// ListManaged returns metadata for the ccmux-managed sessions on the server.
func (s *Server) ListManaged() ([]SessionMeta, error) {
	out, err := s.run("list-sessions", "-F",
		"#{session_name}|#{@ccmux_managed}|#{@ccmux_workspace_id}|#{session_windows}")
	if err != nil {
		if strings.Contains(err.Error(), "no server running") {
			return nil, nil
		}
		return nil, err
	}
	var metas []SessionMeta
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		if line == "" {
			continue
		}
		f := strings.SplitN(line, "|", 4)
		if len(f) != 4 || f[1] != "1" {
			continue // skip non-managed sessions sharing the socket
		}
		w, _ := strconv.Atoi(f[3])
		metas = append(metas, SessionMeta{Name: f[0], Managed: true, WorkspaceID: f[2], Windows: w})
	}
	return metas, nil
}
