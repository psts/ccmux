package main

import (
	"context"
	"errors"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"

	"ccmux.dev/ccmuxd/internal/agent"
	"ccmux.dev/ccmuxd/internal/api"
	"ccmux.dev/ccmuxd/internal/harness"
	"ccmux.dev/ccmuxd/internal/runner"
	"ccmux.dev/ccmuxd/internal/store"
)

// wireRunners starts the session-runner supervisor (internal/runner): Claude
// sessions on the Agent SDK, each in its own systemd user service. Linux
// only, since it rides on systemd; anywhere else, or when systemd cannot be
// reached, the routes answer that runners are off and the daemon goes on.
func wireRunners(ctx context.Context, st *store.SQLite, agents *agent.Store, apiSrv *api.Server) {
	if runtime.GOOS != "linux" {
		return
	}
	if _, err := exec.LookPath("systemctl"); err != nil {
		log.Printf("session runners off: no systemctl on this host")
		return
	}
	paths := runner.Paths{State: runnerStateDir(), Runtime: filepath.Join(runtimeDir(), "s")}
	units := runner.Systemctl{}
	if err := runner.EnsureTemplate(systemdUserDir(), selfExecutable(), paths, units); err != nil {
		log.Printf("session runners off: unit template: %v", err)
		return
	}
	claude, err := harness.LookPath("claude")
	if err != nil {
		// Sessions still run, on the SDK's own copy of Claude Code, which
		// may not be the version the terminal runs.
		log.Printf("session runners: no claude on this host (%v); sessions use the SDK's bundled Claude Code", err)
	}
	cfg := runner.Config{Sidecar: agents.EnsureClaudeServe, Claude: claude, Env: runnerEnv}
	sup := runner.New(st, units, paths, cfg)
	apiSrv.SetRunners(sup, st)
	go sup.Run(ctx)
}

// runnerStateDir holds each session's spec and state file. It must outlive
// a reboot, so it is not under the runtime folder.
func runnerStateDir() string {
	if d := os.Getenv("XDG_STATE_HOME"); d != "" {
		return filepath.Join(d, "ccmux", "sessions")
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".local", "state", "ccmux", "sessions")
}

func systemdUserDir() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".config", "systemd", "user")
}

// runnerEnv is the environment a session starts with: the daemon's own,
// with the user's ~/.local/bin first on PATH. The daemon's service PATH is
// a bare system one (service_linux.go), and Claude's tools (claude itself,
// ccmuxd, ccmux-peers) live in ~/.local/bin.
func runnerEnv() []string {
	home, _ := os.UserHomeDir()
	local := filepath.Join(home, ".local", "bin")
	env := os.Environ()
	for i, kv := range env {
		if strings.HasPrefix(kv, "PATH=") {
			env[i] = "PATH=" + local + string(os.PathListSeparator) + strings.TrimPrefix(kv, "PATH=")
			return env
		}
	}
	return append(env, "PATH="+local+":/usr/local/bin:/usr/bin:/bin")
}

// cmdRunnerExec is the unit template's ExecStart: become the runner a
// session's spec file describes.
func cmdRunnerExec(args []string) error {
	if len(args) != 1 {
		return errors.New("usage: ccmuxd runner-exec SPEC.json")
	}
	return runner.Exec(args[0])
}
