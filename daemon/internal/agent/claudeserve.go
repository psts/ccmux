package agent

import (
	"context"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"ccmux.dev/ccmuxd/internal/harness"
)

// The claude sidecar: a Node program on the Claude Agent SDK that runs an
// agent's Claude Code session and serves its conversation on a loopback
// port in the same shape the daemon reads from opencode. Embedded here,
// written under the agents root at every claude instance start (so a
// daemon upgrade's sidecar lands without ceremony), and its one npm
// dependency installed there once per pinned version.

//go:embed claudeserve/serve.mjs claudeserve/args.mjs claudeserve/agent.mjs claudeserve/routes.mjs claudeserve/translate.mjs claudeserve/package.json claudeserve/package-lock.json
var claudeServeFiles embed.FS

// claudeServeShipped is what EnsureClaudeServe writes: the sidecar, its
// manifest and the lock that pins every package the install may fetch (npm
// ci refuses anything the lock does not name), never the tests.
var claudeServeShipped = []string{claudeServeFile, "args.mjs", "agent.mjs", "routes.mjs", "translate.mjs", "package.json", "package-lock.json"}

// installTimeout bounds npm ci so a stalled registry cannot hold the
// install lock, and every claude start behind it, forever.
const installTimeout = 3 * time.Minute

// ClaudeServeDir is where the store writes the sidecar: under the agents
// root, dot-prefixed so List skips it.
const ClaudeServeDir = ".ccmux/claude-serve"

// claudeServeFile is the sidecar's entry point inside ClaudeServeDir.
const claudeServeFile = "serve.mjs"

// ClaudeServeScript is the sidecar's entry point under root.
func ClaudeServeScript(root string) string {
	return filepath.Join(root, ClaudeServeDir, claudeServeFile)
}

var installMu sync.Mutex

// EnsureClaudeServe writes the sidecar under the agents root and installs
// its dependency when the installed version is not the pinned one. Returns
// the script path and the node binary to run it with. Serialized: two
// instances starting together must not run two installs into one folder.
func (s *Store) EnsureClaudeServe() (script, node string, err error) {
	installMu.Lock()
	defer installMu.Unlock()
	dir := filepath.Join(s.Root, ClaudeServeDir)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", "", err
	}
	for _, name := range claudeServeShipped {
		body, err := claudeServeFiles.ReadFile("claudeserve/" + name)
		if err != nil {
			return "", "", err
		}
		if _, err := writeIfChanged(filepath.Join(dir, name), body); err != nil {
			return "", "", err
		}
	}
	node, err = harness.LookPath("node")
	if err != nil {
		return "", "", fmt.Errorf("the claude harness for agents needs node (Node.js 18+) on this host: %w", err)
	}
	if err := installClaudeServeDeps(dir, node); err != nil {
		return "", "", err
	}
	return ClaudeServeScript(s.Root), node, nil
}

// installClaudeServeDeps runs npm ci in dir unless the installed SDK
// already matches the pinned version in the embedded package.json. ci, not
// install: it takes the shipped lock as the whole truth, integrity hashes
// included, and refuses to resolve anything the lock does not name.
//
// npm is a script that starts with "#!/usr/bin/env node", so node's own
// folder goes on the PATH npm runs with: the daemon runs under systemd with
// the bare system PATH, where LookPath finds npm in ~/.local/bin but env
// then cannot find node for it (exit 127, "/usr/bin/env: 'node': No such
// file or directory"). The meridian package solves the same trap for its
// own npm-started child.
func installClaudeServeDeps(dir, node string) error {
	want, err := pinnedSDKVersion()
	if err != nil {
		return err
	}
	if got := installedSDKVersion(dir); got == want {
		return nil
	}
	npm, err := harness.LookPath("npm")
	if err != nil {
		return fmt.Errorf("the claude harness for agents needs npm on this host to install its sidecar: %w", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), installTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, npm, "ci", "--no-audit", "--no-fund", "--loglevel=error")
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "PATH="+filepath.Dir(node)+string(os.PathListSeparator)+os.Getenv("PATH"))
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("npm ci for the claude sidecar in %s: %w: %s", dir, err, strings.TrimSpace(string(out)))
	}
	if got := installedSDKVersion(dir); got != want {
		return fmt.Errorf("npm ci for the claude sidecar left SDK %q, wanted %q", got, want)
	}
	return nil
}

const sdkPackage = "@anthropic-ai/claude-agent-sdk"

func pinnedSDKVersion() (string, error) {
	body, err := claudeServeFiles.ReadFile("claudeserve/package.json")
	if err != nil {
		return "", err
	}
	var pkg struct {
		Dependencies map[string]string `json:"dependencies"`
	}
	if err := json.Unmarshal(body, &pkg); err != nil {
		return "", err
	}
	v := pkg.Dependencies[sdkPackage]
	if v == "" {
		return "", errors.New("claude sidecar package.json pins no SDK version")
	}
	return v, nil
}

// installedSDKVersion is the version in node_modules, "" when absent.
func installedSDKVersion(dir string) string {
	body, err := os.ReadFile(filepath.Join(dir, "node_modules", sdkPackage, "package.json"))
	if err != nil {
		return ""
	}
	var pkg struct {
		Version string `json:"version"`
	}
	_ = json.Unmarshal(body, &pkg)
	return pkg.Version
}

// ClaudeOfflineSessions lists the Claude Code conversations opened in dir,
// newest first, through the sidecar's offline read (the SDK's session
// store), for a chat view on an asleep claude agent.
func ClaudeOfflineSessions(ctx context.Context, root, dir string) ([]OpencodeSession, error) {
	out, err := runClaudeServe(ctx, root, "sessions", "--dir", dir)
	if err != nil {
		return nil, err
	}
	var raw []struct {
		ID        string `json:"id"`
		Title     string `json:"title"`
		Directory string `json:"directory"`
		Time      struct {
			Updated int64 `json:"updated"`
		} `json:"time"`
	}
	if err := json.Unmarshal(out, &raw); err != nil {
		return nil, fmt.Errorf("claude sessions: %w", err)
	}
	mine := []OpencodeSession{}
	for _, s := range raw {
		if SameDir(s.Directory, dir) {
			mine = append(mine, OpencodeSession{ID: s.ID, Title: s.Title, Directory: s.Directory, Updated: s.Time.Updated})
		}
	}
	sortNewestFirst(mine)
	return mine, nil
}

// ClaudeOfflineTranscript is one conversation through the sidecar's export.
func ClaudeOfflineTranscript(ctx context.Context, root, dir, sessionID string) ([]Turn, error) {
	out, err := runClaudeServe(ctx, root, "export", "--dir", dir, "--session", sessionID)
	if err != nil {
		return nil, err
	}
	return NormalizeExport(out)
}

func runClaudeServe(ctx context.Context, root string, args ...string) ([]byte, error) {
	script := ClaudeServeScript(root)
	if _, err := os.Stat(script); err != nil {
		return nil, fmt.Errorf("claude sidecar not installed yet (it lands at the first claude agent start): %w", err)
	}
	node, err := harness.LookPath("node")
	if err != nil {
		return nil, fmt.Errorf("node not installed: %w", err)
	}
	out, err := exec.CommandContext(ctx, node, append([]string{script}, args...)...).Output()
	if err != nil {
		var exit *exec.ExitError
		if errors.As(err, &exit) && len(exit.Stderr) > 0 {
			return nil, fmt.Errorf("claude sidecar %v: %w: %s", args, err, strings.TrimSpace(string(exit.Stderr)))
		}
		return nil, fmt.Errorf("claude sidecar %v: %w", args, err)
	}
	return out, nil
}
