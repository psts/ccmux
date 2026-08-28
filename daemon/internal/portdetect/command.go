package portdetect

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
)

// DetectCommand guesses the command that starts a repo's dev server, for the
// workspace's ▶ button when no explicit devCommand is stored. Unlike port
// priority, the package.json dev script beats compose here: when both exist,
// the script is the developer entrypoint and compose is often just infra
// (pinned on the admin repo, where `pnpm dev` is how the servers actually run).
// Returns ("", "") when nothing credible is found.
func DetectCommand(dir string) (command, source string) {
	if cmd := scriptCommand(dir); cmd != "" {
		return cmd, "package.json"
	}
	for _, pattern := range []string{"docker-compose*.yml", "docker-compose*.yaml", "compose.yml", "compose.yaml"} {
		if matches, _ := filepath.Glob(filepath.Join(dir, pattern)); len(matches) > 0 {
			return "docker compose up", filepath.Base(matches[0])
		}
	}
	if cmd := procfileWeb(dir); cmd != "" {
		return cmd, "Procfile (verify — often the production command)"
	}
	return "", ""
}

// scriptCommand maps a package.json to its runner invocation: a "dev" script
// via the lockfile-detected package manager, or "start" when it's the CRA-style
// dev server. "" when the manifest has no dev entrypoint.
func scriptCommand(dir string) string {
	raw, err := os.ReadFile(filepath.Join(dir, "package.json"))
	if err != nil {
		return ""
	}
	var pkg struct {
		Scripts map[string]string `json:"scripts"`
	}
	if json.Unmarshal(raw, &pkg) != nil {
		return ""
	}
	runner := packageRunner(dir)
	if pkg.Scripts["dev"] != "" {
		if runner == "npm" {
			return "npm run dev"
		}
		return runner + " dev"
	}
	if strings.Contains(pkg.Scripts["start"], "react-scripts start") {
		return runner + " start"
	}
	return ""
}

// PortFlagSuffix returns what to append to a dev command so the server binds
// the allocated $PORT. Most frameworks (next, react-scripts, node servers)
// honor the PORT env var the pane already carries; vite does not — it needs
// an explicit --port. Only the runner form scriptCommand would produce is
// touched: anything else ("docker compose up", a hand-rolled script) must not
// grow flags it never asked for. "" = append nothing.
func PortFlagSuffix(dir, command string) string {
	if strings.Contains(command, "--port") || strings.Contains(command, "$PORT") {
		return "" // the command already pins its port
	}
	if command == "" || command != scriptCommand(dir) {
		return ""
	}
	raw, err := os.ReadFile(filepath.Join(dir, "package.json"))
	if err != nil {
		return ""
	}
	var pkg struct {
		Scripts map[string]string `json:"scripts"`
	}
	if json.Unmarshal(raw, &pkg) != nil {
		return ""
	}
	fields := strings.Fields(pkg.Scripts["dev"])
	if len(fields) == 0 || fields[0] != "vite" {
		return ""
	}
	for _, f := range fields {
		if f == "--port" || f == "-p" {
			return ""
		}
	}
	// npm forwards script args only after --; pnpm/yarn/bun pass them through.
	if strings.HasPrefix(command, "npm ") {
		return ` -- --port "$PORT"`
	}
	return ` --port "$PORT"`
}

// packageRunner picks the package manager from the lockfile present.
func packageRunner(dir string) string {
	for lock, runner := range map[string]string{
		"pnpm-lock.yaml": "pnpm",
		"yarn.lock":      "yarn",
		"bun.lock":       "bun",
		"bun.lockb":      "bun",
	} {
		if _, err := os.Stat(filepath.Join(dir, lock)); err == nil {
			return runner
		}
	}
	return "npm"
}

// procfileWeb returns the Procfile's web entry, the weakest signal — heroku
// Procfiles usually hold the production command.
func procfileWeb(dir string) string {
	f, err := os.Open(filepath.Join(dir, "Procfile"))
	if err != nil {
		return ""
	}
	defer f.Close()
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		if cmd, ok := strings.CutPrefix(strings.TrimSpace(scanner.Text()), "web:"); ok {
			return strings.TrimSpace(cmd)
		}
	}
	return ""
}
