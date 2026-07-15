package api

import (
	"net/http"
	"os"
	"path/filepath"
	"strings"
)

// projectEntry is one selectable folder under the daemon's projects root — the
// only places a lens can create a hosted workspace from.
type projectEntry struct {
	Name string `json:"name"`
	Path string `json:"path"`
	Git  bool   `json:"git"`
}

// listProjects returns the subdirectories of one folder inside the configured
// projects root: the root itself, or — with ?path=<relative subpath> — any
// folder below it, so lenses can drill into nested project layouts. The
// response carries "path" (the listed folder, relative to the root; "" at the
// root) and "parent" (one level up, absent at the root) for navigation.
//
// Lenses feed their "new hosted session" picker exclusively from this list:
// folder choice happens on the daemon's filesystem (which may be a remote
// server), never on the lens's own disk. Hidden entries are skipped; symlinks
// to directories count.
func (s *Server) listProjects(w http.ResponseWriter, r *http.Request) {
	if s.projectsRoot == "" {
		writeError(w, http.StatusServiceUnavailable, "projects root not configured")
		return
	}
	rel, ok := cleanProjectsPath(r.URL.Query().Get("path"))
	if !ok {
		writeError(w, http.StatusBadRequest, "path must be relative and stay inside the projects root")
		return
	}
	dir := filepath.Join(s.projectsRoot, rel)
	entries, err := os.ReadDir(dir)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	projects := []projectEntry{}
	for _, e := range entries {
		name := e.Name()
		if strings.HasPrefix(name, ".") {
			continue
		}
		path := filepath.Join(dir, name)
		if !e.IsDir() {
			fi, err := os.Stat(path) // follows symlinks
			if err != nil || !fi.IsDir() {
				continue
			}
		}
		// .git may be a directory or, in a linked worktree, a file.
		_, gitErr := os.Stat(filepath.Join(path, ".git"))
		projects = append(projects, projectEntry{Name: name, Path: path, Git: gitErr == nil})
	}
	resp := map[string]any{"root": s.projectsRoot, "path": rel, "projects": projects}
	if rel != "" {
		parent := filepath.Dir(rel)
		if parent == "." {
			parent = ""
		}
		resp["parent"] = parent
	}
	writeJSON(w, http.StatusOK, resp)
}

// cleanProjectsPath normalizes a ?path= value to a relative subpath that cannot
// escape the projects root. "" means the root itself; absolute paths and any
// path whose cleaned form starts with ".." are rejected.
func cleanProjectsPath(raw string) (string, bool) {
	if raw == "" {
		return "", true
	}
	if filepath.IsAbs(raw) {
		return "", false
	}
	rel := filepath.Clean(raw)
	if rel == "." {
		return "", true
	}
	if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", false
	}
	return rel, true
}
