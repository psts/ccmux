package agent

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
)

// A repo session in a shared window publishes into its own folder of the
// window's shared folder (shared/<repo>/). That folder is outside its repo,
// so Claude Code would ask before every write there. The grant goes where
// every way of starting that session reads it: the repo's own
// .claude/settings.local.json, permissions.additionalDirectories. Only a
// file git ignores is touched, so no repo ever shows a change from ccmux.

const repoLocalSettings = ".claude/settings.local.json"

var repoAccessMu sync.Mutex

// GrantRepoDir adds dir to repo's local Claude settings as an additional
// directory, keeping everything else in the file. It reports whether it
// wrote. A repo whose git does not ignore the file, or a file that does not
// parse, is left alone and said so in the error.
func GrantRepoDir(repo, dir string) (bool, error) {
	repoAccessMu.Lock()
	defer repoAccessMu.Unlock()
	if err := gitIgnores(repo, repoLocalSettings); err != nil {
		return false, err
	}
	path := filepath.Join(repo, repoLocalSettings)
	settings, err := readSettings(path)
	if err != nil {
		return false, err
	}
	perms, _ := settings["permissions"].(map[string]any)
	if perms == nil {
		perms = map[string]any{}
	}
	dirs, _ := perms["additionalDirectories"].([]any)
	if slices.Contains(dirs, any(dir)) {
		return false, nil
	}
	perms["additionalDirectories"] = append(dirs, dir)
	settings["permissions"] = perms
	return true, writeSettings(path, settings)
}

// gitIgnores is nil when git ignores rel in repo. Not ignored (exit 1) and
// git failing (not a repo, no git, a fatal error) are told apart, with
// git's own words for the second.
func gitIgnores(repo, rel string) error {
	out, err := exec.Command("git", "-C", repo, "check-ignore", "-q", rel).CombinedOutput()
	var exit *exec.ExitError
	switch {
	case err == nil:
		return nil
	case errors.As(err, &exit) && exit.ExitCode() == 1:
		return fmt.Errorf("%s: git does not ignore %s, so ccmux does not write it", repo, rel)
	}
	return fmt.Errorf("%s: cannot ask git whether %s is ignored: %w %s", repo, rel, err, strings.TrimSpace(string(out)))
}

func readSettings(path string) (map[string]any, error) {
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return map[string]any{}, nil
	}
	if err != nil {
		return nil, err
	}
	var settings map[string]any
	if err := json.Unmarshal(raw, &settings); err != nil || settings == nil {
		return nil, fmt.Errorf("%s does not parse as a JSON object, left as it is: %v", path, err)
	}
	return settings, nil
}

// writeSettings replaces the file in one rename, so Claude Code never reads
// half of it.
func writeSettings(path string, settings map[string]any) error {
	body, err := json.MarshalIndent(settings, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	mode := os.FileMode(0o644)
	if fi, err := os.Stat(path); err == nil {
		mode = fi.Mode().Perm()
	}
	tmp := path + ".ccmux-tmp"
	if err := os.WriteFile(tmp, append(body, '\n'), mode); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
