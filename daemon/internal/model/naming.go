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
	s := SlugText(filepath.Base(strings.TrimRight(repoPath, "/")))
	if len(s) > maxSlugLen {
		s = strings.Trim(s[:maxSlugLen], "-")
	}
	if s == "" {
		return "repo"
	}
	return s
}

// SlugText is the one slug rule: ASCII letters and digits kept (lowercased),
// every other run becomes one dash, dashes at either end dropped. No length
// cap and no fallback; Slug adds those for session names.
func SlugText(text string) string {
	var b strings.Builder
	prevDash := false
	for _, r := range strings.ToLower(text) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
			prevDash = false
		} else if !prevDash {
			b.WriteByte('-')
			prevDash = true
		}
	}
	return strings.Trim(b.String(), "-")
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
