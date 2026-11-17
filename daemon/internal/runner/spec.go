package runner

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
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
	// Brief is the text of the session's brief file (Paths.BriefFile), which
	// Args name: written beside the spec, never into it.
	Brief string `json:"-"`
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

// BriefFile holds a thread member's standing brief (Config.Brief), which its
// runner reads at every start.
func (p Paths) BriefFile(id string) string { return filepath.Join(p.State, id+".brief.md") }

// MediaDir holds a session's images (pasted into a prompt, or returned by a
// tool) as files named after their bytes: the runner writes and reads them,
// the daemon serves them to the session's page and stores an upload there.
func (p Paths) MediaDir(id string) string { return filepath.Join(p.State, id+".media") }

// Config is what every session's spec is built from.
type Config struct {
	// Sidecar installs the runner's script and names it and node.
	Sidecar func() (script, node string, err error)
	// Claude is the host's claude, so sessions run what the terminal runs.
	Claude string
	// Env is the environment a runner starts with, before the session's own.
	Env func() []string
	// SessionEnv is one session's own variables: its pane's identity and
	// the daemon's addresses, the set a tmux pane's shell is given
	// (manager.RunnerEnv). They replace the same names in Env. Nil, or a
	// session with no pane, adds none.
	SessionEnv func(store.RunnerSession) map[string]string
	// Peers is the bus shim (ccmux-peers) a session with a pane loads, as
	// its claude-peers MCP server in place of the one the user's own config
	// names: that one is whatever was last installed, and it joins the bus
	// only from a terminal. "" leaves the user's config to decide.
	Peers string
	// Brief is the standing text a session starts with, on top of Claude
	// Code's own system prompt: for a thread member, who it is in its thread
	// and how the thread works (threads.Brief). Nil, or "" from it, is none.
	Brief func(store.RunnerSession) string
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
		"--media", paths.MediaDir(sess.ID),
	}
	if c.Claude != "" {
		args = append(args, "--claude", c.Claude)
	}
	if sess.Model != "" {
		args = append(args, "--model", sess.Model)
	}
	if c.Peers != "" && sess.PaneID != "" {
		args = append(args, "--peers-command", c.Peers)
	}
	args = append(args, fenceArgs(sess)...)
	brief := ""
	if c.Brief != nil {
		brief = c.Brief(sess)
	}
	if brief != "" {
		args = append(args, "--system-prompt-file", paths.BriefFile(sess.ID))
	}
	return Spec{Node: node, Script: script, Args: args, Dir: sess.Cwd, Env: c.env(sess), Brief: brief}, nil
}

// fenceArgs are a thread member's fences (claudeserve/readonly.mjs): read
// only while it is in its repo's own folder, and kept out of that folder
// once it works in its worktree. A session in no thread has neither.
func fenceArgs(sess store.RunnerSession) []string {
	switch {
	case sess.ReadOnly:
		return []string{"--read-only", "1"}
	case sess.Worktree != "" && sess.Cwd == sess.Worktree && sess.Repo != "":
		return []string{"--keep-out", sess.Repo}
	}
	return nil
}

func sessionLabel(sess store.RunnerSession) string {
	if sess.Name != "" {
		return sess.Name
	}
	return filepath.Base(sess.Cwd)
}

// dropEnv are variables of the daemon's own process that must not reach a
// session: a tmux pane's identity and the addresses that go with it (the
// daemon, its hooks socket, its LLM proxy route), the bus shim's switches,
// and Claude Code's markers of running inside another Claude. A daemon
// started from a ccmux pane (a test, a scratch daemon) carries all of them,
// and a session that kept them would report to, and bill, that pane. The
// session's own values are set after (Config.SessionEnv).
var dropEnv = []string{
	"CLAUDECODE=", "CLAUDE_CODE_ENTRYPOINT=", "TMUX=", "TMUX_PANE=",
	"CCMUX_PANE_ID=", "CCMUX_PANE_TOKEN=", "CCMUX_CMD_FILE=", "CCMUX_DAEMON_URL=", "CCMUX_HOOKS_SOCK=",
	"CCMUX_PEERS_URL=", "CCMUX_PEERS_CHANNEL=", "CCMUX_BUS_OWNER=", "CLAUDE_PEERS_NAME=",
	"ANTHROPIC_BASE_URL=", "_CLAUDE_CODE_ASSUME_FIRST_PARTY_BASE_URL=", "CLAUDE_CODE_MAX_MCP_DESCRIPTION_LENGTH=",
	"OPENCODE_CONFIG_CONTENT=", "ZDOTDIR=",
}

// env is a session's whole environment: the daemon's, without what must
// not leak, then the session's own, each name once.
func (c Config) env(sess store.RunnerSession) []string {
	var base []string
	if c.Env != nil {
		base = c.Env()
	}
	var own map[string]string
	if c.SessionEnv != nil && sess.PaneID != "" {
		own = c.SessionEnv(sess)
	}
	out := make([]string, 0, len(base)+len(own))
	for _, kv := range base {
		name, _, _ := strings.Cut(kv, "=")
		if _, replaced := own[name]; !replaced && !dropped(kv) {
			out = append(out, kv)
		}
	}
	names := make([]string, 0, len(own))
	for name := range own {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		out = append(out, name+"="+own[name])
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

// writeBrief writes a session's brief file, or removes one it no longer has.
func writeBrief(path, text string) error {
	if text == "" {
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			return err
		}
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	return os.WriteFile(path, []byte(text), 0o600)
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
