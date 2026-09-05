package agent

import (
	_ "embed"
	"os"
	"path/filepath"
)

//go:embed ccmux-opencode.ts
var opencodePlugin []byte

// PluginFile is where the store writes the embedded opencode plugin: under
// the agents root, dot-prefixed so List skips it.
const PluginFile = ".ccmux/ccmux-opencode.ts"

// EnsurePlugin writes the embedded opencode plugin (idle/busy signals to the
// daemon) under the agents root and returns its path. Called at every
// instance start so a daemon upgrade's plugin lands without ceremony.
func (s *Store) EnsurePlugin() (string, error) {
	p := filepath.Join(s.Root, PluginFile)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return "", err
	}
	if _, err := writeIfChanged(p, opencodePlugin); err != nil {
		return "", err
	}
	return p, nil
}
