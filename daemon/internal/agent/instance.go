package agent

import (
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"

	"ccmux.dev/ccmuxd/internal/model"
)

// InstanceDir is where a base lives inside one shared window (a project):
// the project's own instructions, skills, memory and log for that agent
// (spec §12). It sits beside the bases, under the window's folder
// (WindowDir), as agents/<name>. A running instance is found again through
// the agent session's RepoPath, never by rebuilding this path.
func (s *Store) InstanceDir(windowID, windowName, name string) string {
	return filepath.Join(s.WindowDir(windowID, windowName), "agents", name)
}

// WindowDir is the window's folder, ~/.ccmux/windows/<slug>-<id prefix>: the
// slug keeps it readable, the id prefix keeps two windows whose names slug
// the same ("Chart Labs", "Chart-Labs") apart. Resolved by lookup, so a
// renamed window keeps its folder: an existing folder ending in the id
// prefix wins, and only a window without one gets a new name. Nothing is
// matched by name alone: a folder that merely shares the slug is not this
// window's, and adopting it would hand its shared/.env to the wrong project.
func (s *Store) WindowDir(windowID, windowName string) string {
	root := filepath.Join(filepath.Dir(s.Root), "windows")
	prefix := idPrefix(windowID)
	entries, err := os.ReadDir(root)
	if err != nil && !os.IsNotExist(err) {
		// Unreadable is not "nothing there": say so, because the fallback
		// below names a fresh folder and the window's memory stays behind.
		log.Printf("windows folder %s unreadable (%v); window %s gets a fresh folder", root, err, windowID)
	}
	for _, e := range entries {
		if e.IsDir() && (e.Name() == prefix || strings.HasSuffix(e.Name(), "-"+prefix)) {
			return filepath.Join(root, e.Name())
		}
	}
	return filepath.Join(root, windowFolder(windowID, windowName))
}

// windowFolder is "<slug>-<first 8 of the id>" (model.SlugText: ASCII
// letters and digits, one dash per other run, none at the ends); just the
// id prefix when the name has no ASCII letters or digits.
func windowFolder(id, name string) string {
	prefix := idPrefix(id)
	slug := model.SlugText(name)
	if slug == "" {
		return prefix
	}
	return slug + "-" + prefix
}

func idPrefix(id string) string {
	if len(id) > 8 {
		return id[:8]
	}
	return id
}

// Bootstrap creates the instance folder dir for base d if it is not there
// yet. Every file is a starter written ONCE; the daemon never writes here
// again — humans and the agent own it from then on.
func Bootstrap(dir string, d Definition) (created bool, err error) {
	if !ValidName(d.Name) {
		return false, fmt.Errorf("agent name %q invalid", d.Name)
	}
	if _, err := os.Stat(filepath.Join(dir, fileAgents)); err == nil {
		return false, nil
	}
	for _, sub := range []string{"", "memory", filepath.Join(".claude", "skills")} {
		if err := os.MkdirAll(filepath.Join(dir, sub), 0o755); err != nil {
			return false, err
		}
	}
	starter := map[string]string{
		fileAgents: fmt.Sprintf("# %s in this project\n\n"+
			"<!-- Written once by ccmux when the agent was added; yours from here on. -->\n\n"+
			"Base role: %s\n\n"+
			"## Project instructions\n\n- Which parts of this project matter to this agent, and who approves its output.\n", d.Name, d.Description),
		fileClaude:                           claudeShim,
		filepath.Join("memory", "MEMORY.md"): "# Memory index\n\nOne line per durable learning; details in topic files beside this one.\n",
		"log.md":                             "# Task log\n\nOne line per task: date, what was done, where it went.\n",
	}
	for name, body := range starter {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			return false, err
		}
	}
	return true, nil
}
