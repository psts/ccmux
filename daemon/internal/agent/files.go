package agent

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// Generated files. Everything else in a base folder belongs to the human.
const (
	fileAgent     = "agent.json"
	fileAgents    = "AGENTS.md"
	fileClaude    = "CLAUDE.md"
	fileChangelog = "CHANGELOG.md"
	fileManifest  = ".claude-plugin/plugin.json"
	fileMCP       = "mcp.json"
)

// claudeShim is the whole CLAUDE.md: Claude Code reads CLAUDE.md, every other
// harness reads AGENTS.md, and the import keeps one source of truth.
const claudeShim = "@AGENTS.md\n"

// Save creates or updates a base folder. It validates, fills defaults,
// writes agent.json, replaces AGENTS.md when Instructions is non-empty,
// keeps CLAUDE.md as the import shim, seeds the folders and mcp.json once,
// and bumps the manifest patch version whenever a generated file changed.
// Returns the stored definition. Never deletes anything.
func (s *Store) Save(d Definition) (Definition, error) {
	if msg := Reject(d); msg != "" {
		return Definition{}, errors.New(msg)
	}
	d = withDefaults(d)
	dir := s.Dir(d.Name)
	for _, sub := range []string{"", "skills", "knowledge", filepath.Dir(fileManifest)} {
		if err := os.MkdirAll(filepath.Join(dir, sub), 0o755); err != nil {
			return Definition{}, err
		}
	}
	changed, err := s.writeGenerated(dir, d)
	if err != nil {
		return Definition{}, err
	}
	if err := s.seedOnce(dir); err != nil {
		return Definition{}, err
	}
	version, err := s.bumpIfChanged(dir, changed)
	if err != nil {
		return Definition{}, err
	}
	d.Version = version
	return s.Get(d.Name)
}

// writeGenerated writes agent.json and (when given) AGENTS.md, reporting
// whether either differs from what was on disk.
func (s *Store) writeGenerated(dir string, d Definition) (changed bool, err error) {
	stored := d
	stored.Version, stored.Instructions = "", "" // not agent.json fields
	blob, _ := json.MarshalIndent(stored, "", "  ")
	c, err := writeIfChanged(filepath.Join(dir, fileAgent), append(blob, '\n'))
	if err != nil {
		return false, err
	}
	changed = c
	if d.Instructions != "" {
		c, err = writeIfChanged(filepath.Join(dir, fileAgents), []byte(d.Instructions))
		if err != nil {
			return false, err
		}
		changed = changed || c
	}
	if _, err := writeIfChanged(filepath.Join(dir, fileClaude), []byte(claudeShim)); err != nil {
		return false, err
	}
	return changed, nil
}

// seedOnce creates the files a fresh base needs and a human then owns.
func (s *Store) seedOnce(dir string) error {
	seeds := map[string]string{
		fileAgents: "# Role\n\nDescribe what this agent does, who it serves, and what it never does.\n",
		fileMCP:    "{}\n",
	}
	for name, body := range seeds {
		p := filepath.Join(dir, name)
		if _, err := os.Stat(p); err == nil {
			continue
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			return err
		}
	}
	return nil
}

// bumpIfChanged raises the manifest patch version when a generated file
// changed (or writes 1.0.0 for a new base) and appends a CHANGELOG line.
func (s *Store) bumpIfChanged(dir string, changed bool) (string, error) {
	cur, err := readVersion(dir)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return "", err
	}
	next := cur
	switch {
	case cur == "":
		next = "1.0.0"
	case changed:
		next = bumpPatch(cur)
	}
	if next == cur {
		return cur, nil
	}
	manifest, _ := json.MarshalIndent(map[string]string{"name": filepath.Base(dir), "version": next}, "", "  ")
	if err := os.WriteFile(filepath.Join(dir, fileManifest), append(manifest, '\n'), 0o644); err != nil {
		return "", err
	}
	line := fmt.Sprintf("- %s %s: %s\n", next, s.now().Format("2006-01-02"), changelogNote(cur))
	return next, appendFile(filepath.Join(dir, fileChangelog), line)
}

func changelogNote(prev string) string {
	if prev == "" {
		return "created"
	}
	return "definition or instructions changed"
}

// bumpPatch turns "1.2.3" into "1.2.4"; anything unparseable restarts at .1.
func bumpPatch(v string) string {
	parts := strings.Split(v, ".")
	if len(parts) != 3 {
		return v + ".1"
	}
	n, err := strconv.Atoi(parts[2])
	if err != nil {
		return v + ".1"
	}
	return parts[0] + "." + parts[1] + "." + strconv.Itoa(n+1)
}

func readVersion(dir string) (string, error) {
	raw, err := os.ReadFile(filepath.Join(dir, fileManifest))
	if err != nil {
		return "", err
	}
	var m struct {
		Version string `json:"version"`
	}
	if err := json.Unmarshal(raw, &m); err != nil {
		return "", fmt.Errorf("plugin.json: %w", err)
	}
	return m.Version, nil
}

func writeIfChanged(path string, body []byte) (bool, error) {
	old, err := os.ReadFile(path)
	if err == nil && string(old) == string(body) {
		return false, nil
	}
	return true, os.WriteFile(path, body, 0o644)
}

func appendFile(path, line string) error {
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = f.WriteString(line)
	return err
}

// Delete removes a base folder entirely — knowledge and skills included.
// Callers confirm with the human first; this is the one destructive call.
func (s *Store) Delete(name string) error {
	if !namePattern.MatchString(name) {
		return fmt.Errorf("agent name %q invalid", name)
	}
	if _, err := os.Stat(filepath.Join(s.Dir(name), fileAgent)); err != nil {
		return ErrNotFound
	}
	return os.RemoveAll(s.Dir(name))
}
