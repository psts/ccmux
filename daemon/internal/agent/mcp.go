package agent

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
)

// MCP servers live in the base's mcp.json in the neutral shape
// { "<name>": { "command", "args", "env" } }. They arrive as the JSON
// snippet every server's readme ships, pasted into the lens: either that
// shape, or the common { "mcpServers": { ... } } wrapper.

// MCPServer is one server as the lenses list it; Env holds names AND values
// as written, so a pasted snippet with a literal key is shown as such.
type MCPServer struct {
	Name    string            `json:"name"`
	Command string            `json:"command"`
	Args    []string          `json:"args,omitempty"`
	Env     map[string]string `json:"env,omitempty"`
}

// MCPServers lists the base's servers, by name.
func (s *Store) MCPServers(agentName string) ([]MCPServer, error) {
	if !ValidName(agentName) {
		return nil, ErrNotFound
	}
	m, err := ReadMCP(s.Dir(agentName))
	if err != nil {
		return nil, err
	}
	out := []MCPServer{}
	for name, e := range m {
		out = append(out, MCPServer{Name: name, Command: e.Command, Args: e.Args, Env: e.Env})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// AddMCP merges a pasted snippet into mcp.json: every server in it is added
// or replaced by name. Returns the names it took. Bumps the base version.
func (s *Store) AddMCP(agentName string, snippet []byte) ([]string, error) {
	if !ValidName(agentName) {
		return nil, ErrNotFound
	}
	incoming, err := parseMCPSnippet(snippet)
	if err != nil {
		return nil, err
	}
	m, err := ReadMCP(s.Dir(agentName))
	if err != nil {
		return nil, err
	}
	var names []string
	for name, e := range incoming {
		m[name] = e
		names = append(names, name)
	}
	sort.Strings(names)
	return names, s.writeMCP(agentName, m, "mcp servers added: "+fmt.Sprint(names))
}

// RemoveMCP drops one server. Bumps the base version.
func (s *Store) RemoveMCP(agentName, server string) error {
	if !ValidName(agentName) {
		return ErrNotFound
	}
	m, err := ReadMCP(s.Dir(agentName))
	if err != nil {
		return err
	}
	if _, ok := m[server]; !ok {
		return ErrNotFound
	}
	delete(m, server)
	return s.writeMCP(agentName, m, "mcp server "+server+" removed")
}

func (s *Store) writeMCP(agentName string, m map[string]mcpEntry, note string) error {
	if m == nil {
		m = map[string]mcpEntry{}
	}
	blob, _ := json.MarshalIndent(m, "", "  ")
	if err := os.WriteFile(filepath.Join(s.Dir(agentName), fileMCP), append(blob, '\n'), 0o644); err != nil {
		return err
	}
	return s.noteBaseChange(agentName, note)
}

// parseMCPSnippet accepts { "mcpServers": {...} } or the bare map, and
// refuses anything without a command per server.
func parseMCPSnippet(snippet []byte) (map[string]mcpEntry, error) {
	var wrapped struct {
		Servers map[string]mcpEntry `json:"mcpServers"`
	}
	if err := json.Unmarshal(snippet, &wrapped); err == nil && len(wrapped.Servers) > 0 {
		return checkMCP(wrapped.Servers)
	}
	var bare map[string]mcpEntry
	if err := json.Unmarshal(snippet, &bare); err != nil {
		return nil, fmt.Errorf("not an MCP snippet: %w", err)
	}
	delete(bare, "mcpServers")
	if len(bare) == 0 {
		return nil, errors.New("the snippet names no server")
	}
	return checkMCP(bare)
}

func checkMCP(m map[string]mcpEntry) (map[string]mcpEntry, error) {
	for name, e := range m {
		if name == "" || e.Command == "" {
			return nil, fmt.Errorf("server %q needs a command", name)
		}
	}
	return m, nil
}
