package api

import (
	"context"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
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

// createProjectReq is the body of POST /v1/projects: the new folder as a path
// relative to the projects root (its parent must already exist), optionally
// git-init'ed for folders that will hold a repo rather than more folders.
type createProjectReq struct {
	Path string `json:"path"`
	Git  bool   `json:"git"`
}

// createProject makes one new folder under the projects root so lenses can
// grow the tree from their "new hosted session" picker without a shell on the
// daemon's host. Mkdir, not MkdirAll: the picker creates one level at a time,
// and a mistyped deep path should error rather than silently build a tree.
func (s *Server) createProject(w http.ResponseWriter, r *http.Request) {
	if s.projectsRoot == "" {
		writeError(w, http.StatusServiceUnavailable, "projects root not configured")
		return
	}
	var req createProjectReq
	if !decodeJSON(w, r, &req) {
		return
	}
	rel, ok := cleanProjectsPath(req.Path)
	if !ok || rel == "" {
		writeError(w, http.StatusBadRequest, "path must name a folder inside the projects root")
		return
	}
	// A dot-name would be invisible to listProjects (hidden entries are
	// skipped), leaving a folder the picker can never show again.
	if strings.HasPrefix(filepath.Base(rel), ".") {
		writeError(w, http.StatusBadRequest, "folder name must not start with a dot")
		return
	}
	dir := filepath.Join(s.projectsRoot, rel)
	if err := os.Mkdir(dir, 0o755); err != nil {
		status := http.StatusInternalServerError
		if os.IsExist(err) {
			status = http.StatusConflict
		}
		writeError(w, status, err.Error())
		return
	}
	if req.Git {
		ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
		defer cancel()
		if msg := gitInit(ctx, dir); msg != "" {
			writeError(w, http.StatusInternalServerError, msg)
			return
		}
	}
	writeJSON(w, http.StatusCreated, projectEntry{Name: filepath.Base(rel), Path: dir, Git: req.Git})
}

// gitInit runs "git init" in a folder createProject just made, and returns ""
// on success or the message to hand back to the lens. That message is the only
// channel the user has here: both lenses render it verbatim and neither can
// reach the daemon's log.
func gitInit(ctx context.Context, dir string) string {
	out, err := exec.CommandContext(ctx, "git", "init", dir).CombinedOutput()
	if err == nil {
		return ""
	}
	// err, not just out: the failures that carry no output at all are exactly
	// the likely ones — git missing from this host's PATH, or the timeout
	// killing the process. Reporting out alone hands the user "git init: ".
	trimmed := strings.TrimSpace(string(out))
	log.Printf("projects: git init %s: %v (%s)", dir, err, trimmed)
	msg := "git init: " + err.Error()
	if trimmed != "" {
		msg += ": " + trimmed
	}
	// The folder was empty a moment ago; removing it keeps the request atomic
	// instead of leaving a half-made project. If that fails, say so — a folder
	// the user was told does not exist would 409 every retry of the same name.
	if rmErr := os.RemoveAll(dir); rmErr != nil {
		log.Printf("projects: rolling back %s after a failed git init: %v (folder left behind)", dir, rmErr)
		msg += " (the folder could not be removed: " + rmErr.Error() + ")"
	}
	return msg
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
