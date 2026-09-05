package agent

import (
	"fmt"
	"os"
	"path/filepath"
)

// InstanceDir is where a base lives inside one project: the project's own
// instructions, skills, memory and log for that agent (spec §12).
func InstanceDir(repo, name string) string {
	return filepath.Join(repo, ".ccmux", "agents", name)
}

// Bootstrap creates the instance folder for base d inside repo if it is not
// there yet. Every file is a starter written ONCE; the daemon never writes
// here again — humans and the agent own it from then on.
func Bootstrap(repo string, d Definition) (dir string, created bool, err error) {
	if !ValidName(d.Name) {
		return "", false, fmt.Errorf("agent name %q invalid", d.Name)
	}
	dir = InstanceDir(repo, d.Name)
	if _, err := os.Stat(filepath.Join(dir, fileAgents)); err == nil {
		return dir, false, nil
	}
	for _, sub := range []string{"", "memory", filepath.Join(".claude", "skills")} {
		if err := os.MkdirAll(filepath.Join(dir, sub), 0o755); err != nil {
			return "", false, err
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
			return "", false, err
		}
	}
	return dir, true, nil
}
