package buscontext

import (
	"strconv"
	"strings"
)

// The cap on the shim's carrier. Claude Code keeps DefaultCap characters of
// an MCP server's instructions and cuts the rest, unless CapEnv in its own
// environment names another length (Claude Code 2.1.280 and later). The
// daemon sets CapEnv to RaisedCapValue in every pane it starts, and the shim,
// which Claude Code starts with that same environment, reads it back to know
// how much room its lists have. A session started outside ccmux has no CapEnv
// and keeps DefaultCap, which is what Fit is for.
//
// RaisedCap is 16384 because CapEnv replaces two limits at once: this one,
// and the 16384 Claude Code allows the description of a tool it loads through
// tool search. Any other number would raise one by lowering the other.
// Measured on Claude Code 2.1.325 (2026-11-14): with CapEnv at 16384, a
// 17011-character instructions text was kept to exactly 16384, and the
// server process did see CapEnv in its environment.
const (
	CapEnv         = "CLAUDE_CODE_MAX_MCP_DESCRIPTION_LENGTH"
	DefaultCap     = 4096
	RaisedCap      = 16384
	RaisedCapValue = "16384"
)

// Cap is the length CapEnv's value stands for: DefaultCap when it is unset
// or is not a whole number above zero.
func Cap(value string) int {
	if n, err := strconv.Atoi(strings.TrimSpace(value)); err == nil && n > 0 {
		return n
	}
	return DefaultCap
}
