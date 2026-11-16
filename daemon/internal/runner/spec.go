package runner

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"ccmux.dev/ccmuxd/internal/store"
)

// Spec is how one session's runner starts: the program, its arguments,
// its folder and its whole environment. The daemon writes it before every
// start; runner-exec reads it and becomes the runner, so the unit template
// never changes per session.
type Spec struct {
	Node   string   `json:"node"`
	Script string   `json:"script"`
	Args   []string `json:"args"`
	Dir    string   `json:"dir"`
	Env    []string `json:"env"`
}

// Paths are where the runner files live. Specs and state files are kept
// in State (it outlives a reboot); sockets in Runtime (it does not need to).
type Paths struct {
	State   string // e.g. ~/.local/state/ccmux/sessions
	Runtime string // e.g. $XDG_RUNTIME_DIR/ccmuxd/s
	Prefix  string // the unit template's name before "@"; "" is DefaultPrefix
}

// DefaultPrefix names the systemd template every session runs under, one
// instance per session id. A test passes its own, so it never touches the
// template a daemon's sessions use.
const DefaultPrefix = "ccmux-session"

func (p Paths) prefix() string {
	if p.Prefix == "" {
		return DefaultPrefix
	}
	return p.Prefix
}

// Template is the unit template's file name.
func (p Paths) Template() string { return p.prefix() + "@.service" }

// Unit is the service of one session.
func (p Paths) Unit(id string) string { return p.prefix() + "@" + id + ".service" }

func (p Paths) SpecFile(id string) string  { return filepath.Join(p.State, id+".json") }
func (p Paths) StateFile(id string) string { return filepath.Join(p.State, id+".state.json") }
func (p Paths) Socket(id string) string    { return filepath.Join(p.Runtime, id+".sock") }

// Config is what every session's spec is built from.
type Config struct {
	// Sidecar installs the runner's script and names it and node.
	Sidecar func() (script, node string, err error)
	// Claude is the host's claude, so sessions run what the terminal runs.
	Claude string
	// Env is the environment a runner starts with, before the session's own.
	Env func() []string
}

// settingSources loads the user's own settings: their plugins, hooks,
// memory and connectors, as in a terminal session.
const settingSources = "user,project,local"

// BuildSpec makes the spec for one session.
func (c Config) BuildSpec(sess store.RunnerSession, paths Paths) (Spec, error) {
	script, node, err := c.Sidecar()
	if err != nil {
		return Spec{}, fmt.Errorf("session runner: %w", err)
	}
	args := []string{
		"--socket", paths.Socket(sess.ID), "--state", paths.StateFile(sess.ID), "--session-auto", sess.ID,
		"--detached", "1", "--setting-sources", settingSources, "--agent", sessionLabel(sess),
	}
	if c.Claude != "" {
		args = append(args, "--claude", c.Claude)
	}
	if sess.Model != "" {
		args = append(args, "--model", sess.Model)
	}
	return Spec{Node: node, Script: script, Args: args, Dir: sess.Cwd, Env: c.env()}, nil
}

func sessionLabel(sess store.RunnerSession) string {
	if sess.Name != "" {
		return sess.Name
	}
	return filepath.Base(sess.Cwd)
}

// dropEnv are variables of the daemon's own process that must not reach a
// session: a tmux pane's identity, and Claude Code's markers of running
// inside another Claude.
var dropEnv = []string{"CLAUDECODE=", "CLAUDE_CODE_ENTRYPOINT=", "CCMUX_PANE_ID=", "CCMUX_PANE_TOKEN=", "CCMUX_CMD_FILE=", "TMUX=", "TMUX_PANE="}

func (c Config) env() []string {
	var base []string
	if c.Env != nil {
		base = c.Env()
	}
	out := make([]string, 0, len(base))
	for _, kv := range base {
		if !dropped(kv) {
			out = append(out, kv)
		}
	}
	return out
}

func dropped(kv string) bool {
	for _, p := range dropEnv {
		if strings.HasPrefix(kv, p) {
			return true
		}
	}
	return false
}

// WriteSpec writes a session's spec whole, through a temporary name.
func WriteSpec(path string, sp Spec) error {
	b, err := json.MarshalIndent(sp, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// ReadSpec reads a spec file.
func ReadSpec(path string) (Spec, error) {
	var sp Spec
	b, err := os.ReadFile(path)
	if err != nil {
		return sp, err
	}
	if err := json.Unmarshal(b, &sp); err != nil {
		return sp, fmt.Errorf("spec %s: %w", path, err)
	}
	if sp.Node == "" || sp.Script == "" {
		return sp, errors.New("spec " + path + " names no node or script")
	}
	return sp, nil
}

// Exec is the runner-exec verb: it reads the spec and replaces this
// process with the runner, so systemd supervises the runner itself.
func Exec(specPath string) error {
	sp, err := ReadSpec(specPath)
	if err != nil {
		return err
	}
	if err := os.Chdir(sp.Dir); err != nil {
		return fmt.Errorf("session folder: %w", err)
	}
	argv := append([]string{sp.Node, sp.Script, "serve"}, sp.Args...)
	return syscall.Exec(sp.Node, argv, sp.Env)
}
