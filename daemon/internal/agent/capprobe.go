package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// A probe is the ground truth of what a thread session loads: one
// throwaway Claude Code session with the host user's own setup, started in
// an empty folder, whose init message lists the plugins (claude.ai's
// synced ones too), the MCP servers (claude.ai connectors too) and the
// tools it brought (claudeserve/probe.mjs). It sends "hi" on haiku, since
// init only comes with a first prompt, and stops as soon as init arrives.

// ProbeResult is what a probe's init message says, as probe.mjs prints it.
type ProbeResult struct {
	Plugins    []ProbePlugin `json:"plugins"`
	MCPServers []ProbeServer `json:"mcpServers"`
	// Tools are the MCP tools' names (mcp__<server>__<tool>).
	Tools []string `json:"tools"`
}

// ProbePlugin is one loaded plugin. Source is name@marketplace, with the
// marketplace "synced" for claude.ai's and "builtin" for Claude Code's own.
type ProbePlugin struct {
	Name    string `json:"name"`
	Path    string `json:"path"`
	Source  string `json:"source"`
	Version string `json:"version"`
}

// ProbeServer is one MCP server. Source is "claudeai" for a connector,
// "plugin" for a plugin's server, else where it is configured ("user").
type ProbeServer struct {
	Name   string `json:"name"`
	Source string `json:"source"`
	Status string `json:"status"`
}

const (
	probeScript  = "probe.mjs"
	probeTimeout = 2 * time.Minute
)

// RunProbe runs one probe with the claude binary at claude.
func (s *Store) RunProbe(ctx context.Context, claude string) (ProbeResult, error) {
	script, node, err := s.EnsureClaudeServe()
	if err != nil {
		return ProbeResult{}, err
	}
	cwd, err := os.MkdirTemp("", "ccmux-probe-")
	if err != nil {
		return ProbeResult{}, err
	}
	defer os.RemoveAll(cwd)
	ctx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, node, filepath.Join(filepath.Dir(script), probeScript), "--cwd", cwd, "--claude", claude)
	out, err := cmd.Output()
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			err = fmt.Errorf("%w: %s", err, strings.TrimSpace(string(ee.Stderr)))
		}
		return ProbeResult{}, fmt.Errorf("probe session: %w", err)
	}
	return parseProbe(out)
}

// parseProbe reads the probe's last output line.
func parseProbe(out []byte) (ProbeResult, error) {
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	var p ProbeResult
	if err := json.Unmarshal([]byte(lines[len(lines)-1]), &p); err != nil {
		return p, fmt.Errorf("probe session printed no init summary: %w", err)
	}
	return p, nil
}

// toolCounts counts a probe's MCP tools per server; nil when no probe ran,
// and then every count is unknown (-1).
type toolCounts []string

func (t toolCounts) prefixed(prefix string) int {
	if t == nil {
		return -1
	}
	n := 0
	for _, name := range t {
		if strings.HasPrefix(name, prefix) {
			n++
		}
	}
	return n
}

// plugin counts the tools of every server of plugin name. None counted is
// unknown: a server that is not connected (needs auth, failed) shows no
// tools in init, and a plugin with no servers then counts none anyway
// (pluginCost).
func (t toolCounts) plugin(name string) int {
	n := t.prefixed("mcp__plugin_" + toolName(name) + "_")
	if n == 0 {
		return -1
	}
	return n
}

// server counts the tools of the server named name.
func (t toolCounts) server(name string) int { return t.prefixed("mcp__" + toolName(name) + "__") }

var notToolChar = regexp.MustCompile(`[^A-Za-z0-9_-]`)

// toolName is a server name as Claude Code writes it inside tool names:
// "claude.ai Gmail" is mcp__claude_ai_Gmail__<tool>.
func toolName(server string) string { return notToolChar.ReplaceAllString(server, "_") }

var notIDChar = regexp.MustCompile(`[^A-Za-z0-9_.-]+`)

// connectorID is the catalog id of a connector: "claude.ai Gmail" is
// claude.ai-Gmail.
func connectorID(name string) string {
	id := strings.Trim(notIDChar.ReplaceAllString(name, "-"), "-.")
	if len(id) > 64 {
		id = id[:64]
	}
	return id
}

// syncedPluginID is the catalog id of a plugin claude.ai syncs:
// claude.ai-<name>, like a connector's. Never the bare name: the same
// plugin often comes from a git source too (chartlabs-kb, saac-soul), and
// both must have a row whichever was read first, under an id that does not
// depend on the order.
func syncedPluginID(name string) string { return connectorID(SourceClaudeAI + " " + name) }

// probeCandidates are the entries a probe adds beside the installed
// plugins: claude.ai's synced plugins and its connectors. A user MCP
// server is named in skipped, since init carries no config for it, and so
// is a plugin built into Claude Code, which has no folder to choose.
func probeCandidates(p ProbeResult) (out []Capability, skipped []string) {
	tools := toolCounts(p.Tools)
	for _, pl := range p.Plugins {
		switch {
		case strings.HasSuffix(pl.Source, "@synced") && filepath.IsAbs(pl.Path):
			out = append(out, Capability{ID: syncedPluginID(pl.Name), Kind: CapPlugin, Path: pl.Path, Source: SourceClaudeAI, Version: pl.Version, Cost: pluginCost(pl.Path, tools.plugin(pl.Name))})
		case strings.HasSuffix(pl.Source, "@builtin"):
			skipped = append(skipped, pl.Name+": built into Claude Code")
		}
	}
	for _, sv := range p.MCPServers {
		switch sv.Source {
		case "claudeai":
			out = append(out, Capability{ID: connectorID(sv.Name), Kind: CapConnector, Connector: sv.Name, Source: SourceClaudeAI, Cost: serverCost(connectedTools(tools, sv))})
		case "plugin":
		default:
			skipped = append(skipped, sv.Name+": a "+sv.Source+" MCP server; a session's init carries no config for it, add it by hand")
		}
	}
	return out, skipped
}

// connectedTools counts a server's tools; one not connected shows none in
// init, which is unknown rather than none.
func connectedTools(t toolCounts, sv ProbeServer) int {
	n := t.server(sv.Name)
	if n == 0 && sv.Status != "connected" {
		return -1
	}
	return n
}
