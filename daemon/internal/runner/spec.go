package runner

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"ccmux.dev/ccmuxd/internal/store"
	"ccmux.dev/ccmuxd/internal/walls"
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
	// Loadout is the session's loadout file (Paths.LoadoutFile), which Args
	// name too, and LoadoutRecord what the session's row keeps of it. Both
	// empty for a session started with no loadout (Config.Loadout nil).
	Loadout       []byte `json:"-"`
	LoadoutRecord string `json:"-"`
	// AgentVersion is the base version a role agent's session starts with
	// (AgentStart.Version), which its row keeps; "" for any other session.
	AgentVersion string `json:"-"`
	// Runs marks the Claude Code and the SDK this start runs (RunsMark),
	// so the refresh can tell a session that started before an update; ""
	// in a spec an older daemon wrote.
	Runs string `json:"runs,omitempty"`
	// Walls is what a walled session's runner checks before it starts
	// (walls.Check), and Plan the walls themselves, which the supervisor
	// writes as the unit's drop-in file (sessionwalls.go). Both nil for a
	// session that runs open.
	Walls *walls.Expect `json:"walls,omitempty"`
	Plan  *walls.Plan   `json:"-"`
}

// Paths are where the runner files live. Specs and state files are kept
// in State (it outlives a reboot); sockets in Runtime (it does not need to).
type Paths struct {
	State   string // e.g. ~/.local/state/ccmux/sessions
	Runtime string // e.g. $XDG_RUNTIME_DIR/ccmuxd/s
	Prefix  string // the unit template's name before "@"; "" is DefaultPrefix
	// UnitDir is the user's systemd folder, where a walled session's drop-in
	// file goes; "" on a daemon that walls none.
	UnitDir string
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

// Socket is where a session's runner listens now: in the session's own
// folder when it has one (a walled session), else beside every other
// session's. The folder is made when a session starts walled and removed
// when it starts open or ends (Supervisor.wall), so its being there says
// which.
func (p Paths) Socket(id string) string {
	if _, err := os.Stat(p.SocketDir(id)); err == nil {
		return p.WalledSocket(id)
	}
	return p.OpenSocket(id)
}

// OpenSocket is the socket of a session that runs open.
func (p Paths) OpenSocket(id string) string { return filepath.Join(p.Runtime, id+".sock") }

// SocketDir is the one folder of Runtime a walled session sees: its socket
// is made there (WalledSocket), not beside the others'. A runner takes a
// prompt from whoever connects to its socket, so a walled session that
// could reach an open session's socket would prompt it, outside the walls.
func (p Paths) SocketDir(id string) string { return filepath.Join(p.Runtime, id+".d") }

// WalledSocket is a walled session's socket.
func (p Paths) WalledSocket(id string) string { return filepath.Join(p.SocketDir(id), "sock") }

// OwnDir is the one folder of State a walled session may write: its state
// file is kept there (WalledStateFile), not beside every other session's
// spec, which a walled session must not be able to rewrite.
func (p Paths) OwnDir(id string) string { return filepath.Join(p.State, id+".own") }

// WalledStateFile is a walled session's state file.
func (p Paths) WalledStateFile(id string) string { return filepath.Join(p.OwnDir(id), "state.json") }

// BriefFile holds a thread member's standing brief (Config.Brief), which its
// runner reads at every start.
func (p Paths) BriefFile(id string) string { return filepath.Join(p.State, id+".brief.md") }

// LoadoutFile holds what a session loads from the capability catalog
// (Config.Loadout), which its runner reads at every start.
func (p Paths) LoadoutFile(id string) string { return filepath.Join(p.State, id+".loadout.json") }

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
	// and how the thread works (threads.Brief); for a task's builder, its
	// whole brief. Nil, or "" from it, is none. An error fails the start,
	// which is tried again: a builder started without its brief would not
	// know what to build.
	Brief func(store.RunnerSession) (string, error)
	// Loadout is what a session loads from the capability catalog, as its
	// group and repo chose (internal/loadout): the file its runner reads
	// (claudeserve/loadout.mjs) and the record its row keeps. An error fails
	// the start, which is tried again: a session started without its
	// loadout would run with everything, or with nothing, and say neither.
	// Nil starts every session with the user's whole Claude setup, as
	// before loadouts.
	Loadout func(store.RunnerSession) (file []byte, record string, err error)
	// Agent is what a role agent's session starts with, from its base as it
	// is now (agent.go). Nil on a daemon with no agents: an agent's session
	// then fails its start, and says so.
	Agent func(store.RunnerSession) (AgentStart, error)
	// Walls is the walls a session starts behind, asked at every start: nil
	// (the func, or its answer) for one that runs open, an error for one
	// that must not start at all (the sandbox is on and this host cannot
	// wall it). The runner's own folders are added to the answer here.
	Walls func(store.RunnerSession) (*walls.Plan, error)
	// RefreshAfter is how long a session runs before it is started again,
	// once idle, onto what is installed now (refresh.go); 0 never.
	RefreshAfter time.Duration
	// StaleLoadouts is asked once per refresh pass for a check that says
	// whether a session's skills and plugins changed since it started
	// (loadout.Snapshot.Stale): such a session is refreshed at its next
	// quiet moment, without waiting for RefreshAfter. Nil, or a nil answer
	// (the choices could not be read), checks nothing.
	StaleLoadouts func() func(store.RunnerSession) bool
	// RunsNow is the RunsMark a session started now would have, asked once
	// per refresh pass: a running session whose spec marks another is
	// refreshed at its next quiet moment too. Nil, or "", checks nothing.
	RunsNow func() string
	// RunnerCode names the session runner's own code in this build
	// (agent.ClaudeServeCodeHash), for RunsMark; "" leaves it out.
	RunnerCode string
}

// RunsMark names what a start runs: the file the claude link points at
// (Claude Code's updater moves the link), the sidecar script, whose folder
// is its SDK version (agent.SidecarScriptNow), and the runner's own code
// (Config.RunnerCode), which a deploy rewrites in place.
func RunsMark(claude, script, code string) string {
	if real, err := filepath.EvalSymlinks(claude); err == nil {
		claude = real
	}
	mark := "claude=" + claude + " sdk=" + script
	if code != "" {
		mark += " code=" + code
	}
	return mark
}

// settingSources loads the user's own settings, as in a terminal session:
// their CLAUDE.md, memory and hooks come this way. Their plugins, skills,
// MCP servers and connectors come this way too, and a session's loadout
// (Config.Loadout) then switches off every one its group and repo did not
// choose.
const settingSources = "user,project,local"

// BuildSpec makes the spec for one session.
func (c Config) BuildSpec(sess store.RunnerSession, paths Paths) (Spec, error) {
	script, node, err := c.Sidecar()
	if err != nil {
		return Spec{}, fmt.Errorf("session runner: %w", err)
	}
	sp := Spec{Node: node, Script: script, Dir: sess.Cwd, Env: c.env(sess), Runs: RunsMark(c.Claude, script, c.RunnerCode)}
	if sess.Kind == store.SessionAgent {
		return c.agentSpec(sp, sess, paths)
	}
	if sp.Plan, err = c.plan(sess, paths); err != nil {
		return Spec{}, err
	}
	files, err := c.attach(&sp, sess, paths)
	if err != nil {
		return Spec{}, err
	}
	sp.Args = append(c.startArgs(sess, paths), files...)
	if sp.Plan != nil {
		sp.Args = walledArgs(sp.Args, sess, paths, *sp.Plan)
	}
	return sp, nil
}

// startArgs are the flags every start of a session has, from its row: where
// its runner listens and keeps its state, what it runs, and its fences.
func (c Config) startArgs(sess store.RunnerSession, paths Paths) []string {
	args := ownArgs(sess, paths)
	if c.Claude != "" {
		args = append(args, "--claude", c.Claude)
	}
	if sess.Model != "" {
		args = append(args, "--model", sess.Model)
	}
	if c.Peers != "" && sess.PaneID != "" {
		args = append(args, "--peers-command", c.Peers)
	}
	args = append(args, runArgs(sess)...)
	return append(args, fenceArgs(sess)...)
}

// ownArgs are the flags of a session's runner itself: where it listens and
// keeps its state, which conversation it runs (its own id, or the one an
// agent has moved on to, whose state file is then its own too), and the
// user's settings.
func ownArgs(sess store.RunnerSession, paths Paths) []string {
	conv := sess.ConversationID()
	return []string{
		"--socket", paths.OpenSocket(sess.ID), "--state", paths.StateFile(conv), "--session-auto", conv,
		"--detached", "1", "--setting-sources", settingSources, "--agent", sessionLabel(sess),
		"--media", paths.MediaDir(sess.ID),
	}
}

// attach reads a session's brief and its loadout into sp, and returns the
// flags that name their files. Each is named only when there is one: a
// session with no brief has no standing prompt, and a nil loadout is a
// session that loads the user's whole Claude setup (a task's builder
// does), whose file is then removed, so the flag would name nothing.
func (c Config) attach(sp *Spec, sess store.RunnerSession, paths Paths) ([]string, error) {
	var args []string
	var err error
	if c.Brief != nil {
		if sp.Brief, err = c.Brief(sess); err != nil {
			return nil, fmt.Errorf("session runner: its brief: %w", err)
		}
	}
	if sp.Brief != "" {
		args = append(args, "--system-prompt-file", paths.BriefFile(sess.ID))
	}
	if c.Loadout != nil {
		if sp.Loadout, sp.LoadoutRecord, err = c.Loadout(sess); err != nil {
			return nil, fmt.Errorf("session runner: %w", err)
		}
	}
	if sp.Loadout != nil {
		args = append(args, "--loadout", paths.LoadoutFile(sess.ID))
	}
	return args, nil
}

// runArgs are what a session's Claude runs with besides its model: its
// effort, its permission mode and its advisor, each only when its row names
// one. A person's own session names none of them unless they picked an
// advisor, and keeps what their settings choose.
func runArgs(sess store.RunnerSession) []string {
	var args []string
	if sess.Effort != "" {
		args = append(args, "--effort", sess.Effort)
	}
	if sess.PermissionMode != "" {
		args = append(args, "--permission-mode", sess.PermissionMode)
	}
	if sess.Advisor != "" {
		args = append(args, "--advisor", sess.Advisor)
	}
	return args
}

// fenceArgs are a thread member's fences (claudeserve/readonly.mjs): read
// only while it is in its repo's own folder, and kept out of that folder
// once it works in its worktree. A session in no thread has neither.
//
// A task's builder is kept out with its file tools only: its brief tells it
// to copy the env files it needs from the repo's own folder, and the full
// fence refuses any shell command that names that folder and is not a plain
// read ("cp <repo>/.env.local ." among them).
func fenceArgs(sess store.RunnerSession) []string {
	inWorktree := sess.Worktree != "" && sess.Cwd == sess.Worktree && sess.Repo != ""
	switch {
	case sess.ReadOnly:
		return []string{"--read-only", "1"}
	case inWorktree && sess.Role == store.RoleBuilder:
		return []string{"--keep-out-files", sess.Repo}
	case inWorktree:
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
	kept := make([]string, 0, len(base))
	for _, kv := range base {
		if !dropped(kv) {
			kept = append(kept, kv)
		}
	}
	return withEnv(kept, own)
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

// writeLoadout writes a session's loadout file whole, through a temporary
// name, or removes one it no longer has (nil). An empty loadout is still a
// file: "load nothing" is not "no loadout".
func writeLoadout(path string, body []byte) error {
	if body == nil {
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			return err
		}
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, body, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
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
// process with the runner, so systemd supervises the runner itself. A
// walled session's walls are proved first (walls.Check): the error of one
// that is not behind them is a walls.ErrNoWalls, which the verb turns into
// exit status walls.ExitRefused, and nothing of the session runs.
func Exec(specPath string) error {
	sp, err := ReadSpec(specPath)
	if err != nil {
		return err
	}
	if sp.Walls != nil {
		if err := walls.Check(*sp.Walls); err != nil {
			return err
		}
	}
	if err := os.Chdir(sp.Dir); err != nil {
		return fmt.Errorf("session folder: %w", err)
	}
	argv := append([]string{sp.Node, sp.Script, "serve"}, sp.Args...)
	return syscall.Exec(sp.Node, argv, sp.Env)
}
