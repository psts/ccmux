package agent

import (
	"os"
	"path/filepath"
)

// SharedFolder is the window's folder every agent added to it reads and
// writes: what one agent learned that another in the same project needs.
// It sits beside agents/ in the window folder. Its .env (and the one in an
// instance folder) is what the daemon loads into an agent's environment at
// start (EnvFile, launch.go's envPrefix): secrets live there, never in notes.
const (
	SharedFolder = "shared"
	EnvFile      = ".env"
)

// SharedDir is the window's shared folder, whether or not it exists yet.
func (s *Store) SharedDir(windowID, windowName string) string {
	return filepath.Join(s.WindowDir(windowID, windowName), SharedFolder)
}

// ExistingSharedDir is SharedDir when the folder is there, "" before any
// agent has started in the window (which is what creates it).
func (s *Store) ExistingSharedDir(windowID, windowName string) string {
	dir := s.SharedDir(windowID, windowName)
	if fi, err := os.Stat(dir); err != nil || !fi.IsDir() {
		return ""
	}
	return dir
}

// EnsureShared creates the window's shared folder with a README once and
// returns its path. Like Bootstrap, the starter is written ONCE; the daemon
// never touches the folder again.
func (s *Store) EnsureShared(windowID, windowName string) (string, error) {
	dir := s.SharedDir(windowID, windowName)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	readme := filepath.Join(dir, "README.md")
	if _, err := os.Stat(readme); err == nil {
		return dir, nil
	}
	return dir, os.WriteFile(readme, []byte(sharedReadme), 0o644)
}

const sharedReadme = "# Shared by every agent in this window\n\n" +
	"<!-- Written once by ccmux; yours from here on. -->\n\n" +
	"Any agent added to this window reads and writes here. Put what another agent in this project needs.\n\n" +
	"A `.env` beside this file is loaded into every agent's environment at its next start: one KEY=VALUE per line, the value taken as-is " +
	"(surrounding quotes stripped, no shell syntax, nothing runs). " +
	"An agent's own folder may hold a `.env` too; that one wins. Secrets go in `.env`, never in notes or memory.\n"
