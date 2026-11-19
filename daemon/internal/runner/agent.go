package runner

import (
	"errors"
	"fmt"
	"sort"
	"strings"

	"ccmux.dev/ccmuxd/internal/store"
)

// A role agent's session (task #177). A role agent on the claude harness
// used to be the same sidecar typed into a tmux pane, serving a loopback
// port; it is now a runner session like any other, under its own unit, and
// tmux is no longer what keeps it alive. What it starts with is still its
// base's to say, so its spec is built from the base as it is at that start
// (Config.Agent), never from the row: a base edited while the agent slept
// is the one it wakes on.
//
// What differs from a person's session:
//
//   - The SDK's own Claude Code runs it, as it did in the pane, not the
//     host's: the agents page says which version agents run, and an
//     isolated base was checked against that one.
//   - It has no loadout and no brief of the daemon's: its base names its
//     plugins, its MCP servers and its instructions.
//   - Its flags come last on the line, where the runner's own would
//     otherwise win: an isolated base's setting sources replace the user's.
//   - Its .env files' variables are in its environment. They were read by
//     the env-exec verb in the pane; here they are in the spec, which is
//     why the spec is only its owner's to read.

// AgentStart is what one start of a role agent's session adds to the
// runner's own flags: the base's flags as the sidecar reads them
// (agent.ClaudeRunnerArgs), the variables of its .env files, the model its
// base pins ("" for the default) and the base's version.
type AgentStart struct {
	Args    []string
	Env     map[string]string
	Model   string
	Version string
}

// agentSpec finishes sp for a role agent's session.
func (c Config) agentSpec(sp Spec, sess store.RunnerSession, paths Paths) (Spec, error) {
	if c.Agent == nil {
		return Spec{}, errors.New("session runner: this daemon starts no role agents")
	}
	start, err := c.Agent(sess)
	if err != nil {
		return Spec{}, fmt.Errorf("session runner: agent %s: %w", sessionLabel(sess), err)
	}
	args := ownArgs(sess, paths)
	if start.Model != "" {
		args = append(args, "--model", start.Model)
	}
	if c.Peers != "" && sess.PaneID != "" {
		args = append(args, "--peers-command", c.Peers)
	}
	sp.Args = append(append(args, runArgs(sess)...), start.Args...)
	sp.Env = withEnv(sp.Env, start.Env)
	sp.AgentVersion = start.Version
	return sp, nil
}

// withEnv is env with own's variables, each replacing the one of its name.
func withEnv(env []string, own map[string]string) []string {
	out := make([]string, 0, len(env)+len(own))
	for _, kv := range env {
		name, _, _ := strings.Cut(kv, "=")
		if _, replaced := own[name]; !replaced {
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
