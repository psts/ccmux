package model

import (
	"path/filepath"
	"strings"
)

// maxSlugLen bounds the human-readable portion of a tmux session name.
const maxSlugLen = 24

// Slug derives a short, tmux-safe, human-greppable token from a repo path's
// basename: lowercased, non-alphanumeric runs collapsed to single dashes,
// trimmed, length-capped. Empty/degenerate input yields "repo".
func Slug(repoPath string) string {
	base := filepath.Base(strings.TrimRight(repoPath, "/"))
	var b strings.Builder
	prevDash := false
	for _, r := range strings.ToLower(base) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
			prevDash = false
		} else if !prevDash {
			b.WriteByte('-')
			prevDash = true
		}
	}
	s := strings.Trim(b.String(), "-")
	if len(s) > maxSlugLen {
		s = strings.Trim(s[:maxSlugLen], "-")
	}
	if s == "" {
		return "repo"
	}
	return s
}

// SessionName builds the tmux session name `ccmux-<slug>-<uuid8>`. The slug is
// for humans (`tmux ls`); the uuid8 (first 8 hex chars of the workspace id, with
// dashes stripped) guarantees uniqueness. The stable IDs of record live in tmux
// user options (@ccmux_workspace_id / @ccmux_pane_id), not in this name.
func SessionName(slug, workspaceID string) string {
	short := strings.ReplaceAll(workspaceID, "-", "")
	if len(short) > 8 {
		short = short[:8]
	}
	return "ccmux-" + slug + "-" + short
}
