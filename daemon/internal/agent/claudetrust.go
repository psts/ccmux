package agent

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
)

// A headless Claude Code session (the SDK sidecar) never shows the trust
// and external-include dialogs a TUI session shows on a new folder. It
// does not fail without them: it silently DROPS every external @-import
// in the folder's CLAUDE.md / AGENTS.md, which is how an instance shares
// its brief from another repo. The answers live per folder in
// ~/.claude.json, so the daemon gives them at every claude start.

// claudeProjectFlags are the three per-project answers Claude Code records
// when a human accepts the trust dialog and the external-includes warning.
var claudeProjectFlags = []string{
	"hasTrustDialogAccepted",
	"hasClaudeMdExternalIncludesApproved",
	"hasClaudeMdExternalIncludesWarningShown",
}

// ClaudeConfigPath is Claude Code's per-user state file under home.
func ClaudeConfigPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".claude.json"), nil
}

var claudeTrustMu sync.Mutex

// ApproveClaudeProject marks dir as trusted with external includes approved
// in the Claude Code config at configPath, keeping everything else in the
// file. A missing file becomes a minimal one; a file that does not parse
// is left alone and reported, since rewriting it would cost the user the
// rest of their Claude Code state. Claude Code rewrites this file itself
// while it runs, so a concurrent write can lose one side's change: the
// flags are re-applied at every start, which is enough.
func ApproveClaudeProject(configPath, dir string) error {
	if configPath == "" {
		return errors.New("claude config: home folder unknown, cannot mark the instance trusted")
	}
	// Two instances starting together (one bus message can wake two)
	// must not each read the old file and rename over the other's flags.
	claudeTrustMu.Lock()
	defer claudeTrustMu.Unlock()
	cfg, err := readClaudeConfig(configPath)
	if err != nil {
		return err
	}
	projects, _ := cfg["projects"].(map[string]any)
	if projects == nil {
		projects = map[string]any{}
		cfg["projects"] = projects
	}
	project, _ := projects[dir].(map[string]any)
	if project == nil {
		project = map[string]any{}
		projects[dir] = project
	}
	changed := false
	for _, flag := range claudeProjectFlags {
		if v, _ := project[flag].(bool); !v {
			project[flag] = true
			changed = true
		}
	}
	if !changed {
		return nil
	}
	return writeClaudeConfig(configPath, cfg)
}

func readClaudeConfig(path string) (map[string]any, error) {
	body, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return map[string]any{}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("claude config: %w", err)
	}
	var cfg map[string]any
	if err := json.Unmarshal(body, &cfg); err != nil || cfg == nil {
		return nil, fmt.Errorf("claude config %s does not parse, left untouched: %v", path, err)
	}
	return cfg, nil
}

// writeClaudeConfig replaces the file in one rename, so a reader (Claude
// Code itself) never sees a half-written one.
func writeClaudeConfig(path string, cfg map[string]any) error {
	body, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".claude.json.*")
	if err != nil {
		return fmt.Errorf("claude config: %w", err)
	}
	if _, err := tmp.Write(body); err != nil {
		tmp.Close()
		os.Remove(tmp.Name())
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmp.Name())
		return err
	}
	if err := os.Chmod(tmp.Name(), 0o600); err != nil {
		os.Remove(tmp.Name())
		return err
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		os.Remove(tmp.Name())
		return fmt.Errorf("claude config: %w", err)
	}
	return nil
}
