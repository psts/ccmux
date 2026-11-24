package memtidy

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"strings"
	"time"
)

// Headless runs the tidy-up's Claude in its work folder, cut off after
// limit: file tools only (no shell, no web, no MCP servers, none of the
// person's settings, hooks or plugins), and nothing saved of its own
// conversation. No tool is on the allow list, on purpose: a tool listed
// there is taken on any path (tested 2026-11-24: a Write landed in home).
// Unlisted, reads and edits are taken inside the folder (acceptEdits)
// and anything outside it asks, which print mode refuses (tested: a Read,
// Grep and Glob of home and of a sibling folder were refused). It reads
// conversations a walled session wrote, so never list a tool here.
func Headless(claude, model string, env []string, limit time.Duration) func(ctx context.Context, dir, prompt string) (string, error) {
	return func(ctx context.Context, dir, prompt string) (string, error) {
		ctx, cancel := context.WithTimeout(ctx, limit)
		defer cancel()
		cmd := exec.CommandContext(ctx, claude, "-p", prompt,
			"--model", model,
			"--setting-sources", "project",
			"--strict-mcp-config",
			"--permission-mode", "acceptEdits",
			"--disallowedTools", "Bash,WebFetch,WebSearch,Task,Agent,NotebookEdit",
			"--no-session-persistence",
		)
		cmd.Dir = dir
		cmd.Env = env
		// A child of claude holding its output open must not keep Run
		// waiting past the limit.
		cmd.WaitDelay = 10 * time.Second
		var out, errOut bytes.Buffer
		cmd.Stdout, cmd.Stderr = &out, &errOut
		if err := cmd.Run(); err != nil {
			return "", fmt.Errorf("claude: %w: %s", err, lastLine(errOut.String()+"\n"+out.String()))
		}
		return out.String(), nil
	}
}

func lastLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.LastIndexByte(s, '\n'); i >= 0 {
		return s[i+1:]
	}
	return s
}
