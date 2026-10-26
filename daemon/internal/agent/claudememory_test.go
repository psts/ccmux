package agent

import (
	"path/filepath"
	"testing"
)

// The name Claude Code gave radar-seo's live memory store on this host.
func TestClaudeMemoryDir_NamesTheStoreLikeClaudeCode(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", "")
	t.Setenv("HOME", "/home/sanlabs")
	got, err := ClaudeMemoryDir("/home/sanlabs/.ccmux/windows/chartlabs-a86926c5/agents/radar-seo")
	want := "/home/sanlabs/.claude/projects/-home-sanlabs--ccmux-windows-chartlabs-a86926c5-agents-radar-seo/memory"
	if err != nil || got != want {
		t.Fatalf("ClaudeMemoryDir = %q %v, want %q", got, err, want)
	}
	t.Setenv("CLAUDE_CONFIG_DIR", "/etc/claude-x")
	if got, _ := ClaudeMemoryDir("/w/a_b"); got != filepath.Join("/etc/claude-x", "projects", "-w-a-b", "memory") {
		t.Fatalf("with CLAUDE_CONFIG_DIR = %q", got)
	}
}
