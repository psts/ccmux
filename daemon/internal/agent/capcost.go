package agent

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
)

// Cost estimates, in tokens, what one catalog entry adds to the context of
// every session that loads it, before any work is done. Claude Code lists
// every skill, command and subagent by its name and description, and
// sends every MCP tool's definition with each request; a skill's or
// command's body loads only when it is used. So:
//
//   - text counts bytesPerToken (4) bytes per token, the usual average for
//     English prose;
//   - a skill, command or subagent counts its frontmatter name and
//     description;
//   - an MCP tool counts toolTokens (150), a middle value for one tool's
//     name, description and input schema;
//   - a server whose tools nothing has counted (a probe session counts
//     them, capprobe.go) is taken to have unknownServerTools (10) tools.
//
// It is a guide for comparing entries, not a bill.
const (
	bytesPerToken      = 4
	toolTokens         = 150
	unknownServerTools = 10
)

func textTokens(s string) int { return (len(s) + bytesPerToken - 1) / bytesPerToken }

// listedTokens is what the name and description of each file's
// frontmatter cost; an unreadable file costs nothing.
func listedTokens(files []string) int {
	n := 0
	for _, f := range files {
		body, err := os.ReadFile(f)
		if err != nil {
			continue
		}
		fm := frontmatter(string(body))
		n += textTokens(fm["name"] + " " + fm["description"])
	}
	return n
}

// skillCost is one skill folder's cost.
func skillCost(dir string) int {
	return listedTokens([]string{filepath.Join(dir, skillFile)})
}

// pluginCost is a plugin folder's cost: its skills, commands and subagents,
// plus its MCP tools. tools is the plugin's counted MCP tools, or -1 when
// nothing counted them: then each server the plugin declares counts
// unknownServerTools.
func pluginCost(dir string, tools int) int {
	var files []string
	for _, pattern := range []string{"skills/*/" + skillFile, "commands/*.md", "agents/*.md"} {
		m, _ := filepath.Glob(filepath.Join(dir, pattern))
		files = append(files, m...)
	}
	if tools < 0 {
		tools = pluginServers(dir) * unknownServerTools
	}
	return listedTokens(files) + tools*toolTokens
}

// pluginServers counts the MCP servers a plugin declares, in its .mcp.json
// or inline in its manifest.
func pluginServers(dir string) int {
	var doc struct {
		MCPServers map[string]json.RawMessage `json:"mcpServers"`
	}
	n := 0
	if readJSONIfPresent(filepath.Join(dir, ".mcp.json"), &doc) == nil {
		n += len(doc.MCPServers)
	}
	doc.MCPServers = nil
	raw, err := os.ReadFile(filepath.Join(dir, fileManifest))
	if err == nil && json.Unmarshal(raw, &doc) == nil {
		n += len(doc.MCPServers)
	}
	return n
}

// serverCost is one MCP server's cost from its counted tools, -1 unknown.
func serverCost(tools int) int {
	if tools < 0 {
		tools = unknownServerTools
	}
	return tools * toolTokens
}

// manifestVersion is a plugin folder's manifest version, "" when it has none.
func manifestVersion(dir string) string {
	var m struct {
		Version string `json:"version"`
	}
	raw, err := os.ReadFile(filepath.Join(dir, fileManifest))
	if err != nil || json.Unmarshal(raw, &m) != nil {
		return ""
	}
	return strings.TrimSpace(m.Version)
}
