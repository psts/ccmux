package memtidy

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log"
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
//
// It answers in JSON (--output-format json), which carries what the run
// cost besides its answer.
func Headless(claude, model string, env []string, limit time.Duration) func(ctx context.Context, dir, prompt string) (Answer, error) {
	return func(ctx context.Context, dir, prompt string) (Answer, error) {
		ctx, cancel := context.WithTimeout(ctx, limit)
		defer cancel()
		cmd := exec.CommandContext(ctx, claude, "-p", prompt,
			"--model", model,
			"--setting-sources", "project",
			"--strict-mcp-config",
			"--permission-mode", "acceptEdits",
			"--disallowedTools", "Bash,WebFetch,WebSearch,Task,Agent,NotebookEdit",
			"--no-session-persistence",
			"--output-format", "json",
		)
		cmd.Dir = dir
		cmd.Env = env
		// A child of claude holding its output open must not keep Run
		// waiting past the limit.
		cmd.WaitDelay = 10 * time.Second
		var out, errOut bytes.Buffer
		cmd.Stdout, cmd.Stderr = &out, &errOut
		err := cmd.Run()
		return answerOf(out.Bytes(), errOut.String(), err)
	}
}

// Answer is what the tidy-up's Claude said, and what its run cost (list
// price, as Claude Code reports it; 0 when it said nothing about it).
type Answer struct {
	Text    string
	CostUSD float64
}

// result is the object `claude -p --output-format json` prints (read off
// Claude Code 2.1, 2026-11-25).
type result struct {
	Result  *string `json:"result"`
	Cost    float64 `json:"total_cost_usd"`
	IsError bool    `json:"is_error"`
	Subtype string  `json:"subtype"`
}

// answerOf reads Claude's output. A run that failed, or says it did
// (is_error), is an error that still carries the cost. One that worked
// but printed something else is taken as the answer with no cost known:
// Claude Code updates itself, and a new format must not fail every night.
func answerOf(out []byte, errOut string, runErr error) (Answer, error) {
	var r result
	parsed := json.Unmarshal(bytes.TrimSpace(out), &r) == nil && (r.Result != nil || r.Subtype != "")
	a := Answer{CostUSD: r.Cost}
	if r.Result != nil {
		a.Text = *r.Result
	}
	why := a.Text
	if why == "" {
		why = r.Subtype
	}
	if !parsed {
		why = lastLine(errOut + "\n" + string(out))
	}
	switch {
	case runErr != nil:
		return a, fmt.Errorf("claude: %w: %s", runErr, clip(why, 300))
	case r.IsError:
		return a, fmt.Errorf("claude: %s", clip(why, 300))
	case !parsed:
		log.Printf("memory tidy: claude's answer is not the JSON it should be, so its cost is not known")
		return Answer{Text: string(out)}, nil
	}
	return a, nil
}

func lastLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.LastIndexByte(s, '\n'); i >= 0 {
		return s[i+1:]
	}
	return s
}
