package walls

import (
	"os"
	"path/filepath"
)

// Claude Code's own folder (~/.claude) behind the walls.
//
// Every session of the person reads that folder: their settings and its
// hooks, the hook scripts, the plugins, the skills, commands and agents,
// their CLAUDE.md, and the MCP servers in ~/.claude.json. A walled unit
// that could write any of it would run code, or give orders, in the next
// session that starts outside the walls. So the folder is not opened. It
// stays read-only like the rest of home, and of what is in it the plan
// opens a list, never the other way round: a folder Claude Code reads that
// is not there yet (~/.claude/agents on a host that has none) could not be
// sealed by name, and would simply be made.
//
// What a turn writes there was traced on a real one behind walls
// (2026-11-20, Claude Code 2.1.329, strace, with a commit and the commit
// review a hook starts):
//
//   - projects/ holds the conversations and the memory. It is opened: a
//     session that cannot save its conversation cannot be resumed.
//   - shell-snapshots/ and session-env/ hold shell files a session sources
//     before every command it runs. A walled unit that could write another
//     session's would run code in it, so each walled unit gets folders of
//     its own in their place (Plan.Private), and sessions/ (which names
//     each session's own socket) and the rest of what a turn may write
//     with them.
//   - Everything else it tried was refused and cost nothing: the plugins'
//     bookkeeping (in-use marks, a lock beside known_marketplaces.json),
//     and ~/.claude.json, which it saves through a lock folder and a
//     temporary file in home itself. The turn, the commit and the review
//     all worked.
//
// The login is not renewed from behind walls: Claude Code takes a lock in
// ~/.claude before it sends the refresh token, cannot make it there, and
// sends nothing (read in its code, 2.1.329). So a walled session never
// uses up a refresh token it could not then save; it reads the login again
// when a session outside the walls has renewed it.

// claudeData is what of ~/.claude a walled unit writes for real: the
// conversations and the memory.
const claudeData = "projects"

// claudePrivate are the folders of ~/.claude a walled unit gets its own
// of: the first three are needed for a turn to work at all.
var claudePrivate = []string{
	"shell-snapshots", "session-env", "sessions",
	"file-history", "todos", "tasks", "plans", "paste-cache", "debug", "telemetry", "statsig",
}

// claudeNeeded are the folders of ~/.claude a turn cannot do without. A
// walled unit cannot make them (the folder they are in is read-only), so
// the daemon does, before the first walled start (MakeClaudeFolders).
var claudeNeeded = append([]string{claudeData}, claudePrivate[:3]...)

// claudeConfig is what of Claude Code's the walls name as sealed, when it
// is there: what the person's other sessions run or obey. The folder being
// read-only already seals all of it, and whatever is not on this list; the
// names are here so the unit file still seals them when a person opens
// more of home (Settings > Sandbox, "Extra folders"), and so the unit's
// own check tries a write at each.
func claudeConfig(home string) []string {
	dir := filepath.Join(home, ".claude")
	out := []string{filepath.Join(home, ".claude.json")}
	for _, name := range []string{
		"settings.json", "settings.local.json", "CLAUDE.md", "keybindings.json", "statusline.sh",
		"hooks", "plugins", "commands", "agents", "skills", "output-styles", "rules",
	} {
		out = append(out, filepath.Join(dir, name))
	}
	return out
}

// ClaudeHome walls Claude Code's own folder under home into the plan. The
// plan's PrivateRoot must name a folder of the unit's own before it is
// written.
func (p *Plan) ClaudeHome(home string) {
	dir := filepath.Join(home, ".claude")
	p.Caches = append(p.Caches, filepath.Join(dir, claudeData))
	for _, name := range claudePrivate {
		p.Private = append(p.Private, existing(filepath.Join(dir, name))...)
	}
	p.Sealed = append(p.Sealed, existing(claudeConfig(home)...)...)
	p.Outside = append(p.Outside, existing(dir)...)
}

// MakeClaudeFolders makes the folders of ~/.claude a walled turn needs,
// where Claude Code has a folder at all. They are the ones Claude Code
// makes itself at its first turn; behind walls it cannot.
func MakeClaudeFolders(home string) error {
	dir := filepath.Join(home, ".claude")
	if _, err := os.Stat(dir); err != nil {
		return nil // no Claude Code on this host: nothing to prepare
	}
	for _, name := range claudeNeeded {
		if err := os.MkdirAll(filepath.Join(dir, name), 0o700); err != nil {
			return err
		}
	}
	return nil
}
