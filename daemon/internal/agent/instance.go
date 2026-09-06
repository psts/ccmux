package agent

import (
	"fmt"
	"os"
	"path/filepath"

	"ccmux.dev/ccmuxd/internal/model"
)

// InstanceDir is where a base lives inside one shared window (a project):
// the project's own instructions, skills, memory and log for that agent
// (spec §12). It sits beside the bases,
// ~/.ccmux/windows/<window-slug>-<id prefix>/agents/<name>: the slug keeps
// the folder readable, the id prefix keeps two windows whose names slug the
// same ("Chart Labs", "Chart-Labs") apart. The folder is found again through
// the agent session's RepoPath, never by rebuilding this path, so a window
// rename does not lose memory.
func (s *Store) InstanceDir(windowID, windowName, name string) string {
	return filepath.Join(filepath.Dir(s.Root), "windows", windowFolder(windowID, windowName), "agents", name)
}

// windowFolder is "<slug>-<first 8 of the id>" (model.SlugText: ASCII
// letters and digits, one dash per other run, none at the ends); just the
// id prefix when the name has no ASCII letters or digits.
func windowFolder(id, name string) string {
	prefix := id
	if len(prefix) > 8 {
		prefix = prefix[:8]
	}
	slug := model.SlugText(name)
	if slug == "" {
		return prefix
	}
	return slug + "-" + prefix
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
