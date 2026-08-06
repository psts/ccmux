package api

import (
	"errors"
	"io/fs"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"unicode/utf8"
)

// The file routes back the lenses' file viewer for hosted workspaces: the repo
// lives on the daemon's host, so a lens cannot read it locally (same reason the
// manager computes git status daemon-side). Scope is deliberately narrow — text
// files inside the workspace's repo root, human-editor sized.

// maxFileBytes caps what the file routes read or write. The viewer is a text
// editor, not a transfer channel.
const maxFileBytes = 5 << 20

var errPathEscapes = errors.New("path escapes workspace root")

// resolveWorkspacePath maps a lens-supplied path (absolute or repo-relative) to
// an absolute path confined to the workspace root. Symlinks are resolved so a
// link inside the repo cannot escape it; the target must exist.
func resolveWorkspacePath(root, p string) (string, error) {
	rootAbs, err := filepath.EvalSymlinks(filepath.Clean(root))
	if err != nil {
		return "", os.ErrNotExist
	}
	joined := filepath.Clean(p)
	if !filepath.IsAbs(joined) {
		joined = filepath.Join(rootAbs, joined)
	}
	resolved, err := filepath.EvalSymlinks(joined)
	if err != nil {
		return "", os.ErrNotExist
	}
	if resolved != rootAbs && !strings.HasPrefix(resolved, rootAbs+string(filepath.Separator)) {
		return "", errPathEscapes
	}
	return resolved, nil
}

// resolveWorkspaceFile resolves a FILE like resolveWorkspacePath, but a miss
// falls back to a unique suffix match against the repo's git file list.
// Terminals print paths relative to whatever cwd the printing tool had
// ("routers/agents/content.py" for a file at "apps/api/app/routers/…"), and
// neither the lens nor this daemon can recover that cwd — a unique suffix is
// the only safe interpretation. Ambiguous or absent stays a miss.
func resolveWorkspaceFile(root, p string) (string, error) {
	abs, err := resolveWorkspacePath(root, p)
	if err == nil || !errors.Is(err, os.ErrNotExist) {
		return abs, err
	}
	rel := strings.TrimPrefix(filepath.Clean(p), filepath.Clean(root)+string(filepath.Separator))
	match, ok := suffixMatch(root, rel)
	if !ok {
		return "", os.ErrNotExist
	}
	return resolveWorkspacePath(root, match)
}

// suffixMatch finds the single repo file ending in rel, trying progressively
// shorter suffixes (dropping leading segments — they may be cwd components the
// lens wrongly joined on). More than one candidate is ambiguous, not a match.
func suffixMatch(root, rel string) (string, bool) {
	out, err := exec.Command("git", "-C", root, "ls-files", "--cached", "--others", "--exclude-standard").Output()
	if err != nil {
		return "", false
	}
	files := strings.Split(strings.TrimRight(string(out), "\n"), "\n")
	segs := strings.Split(strings.Trim(filepath.ToSlash(rel), "/"), "/")
	for i := 0; i < len(segs); i++ {
		suffix := strings.Join(segs[i:], "/")
		var found string
		for _, f := range files {
			if f == suffix || strings.HasSuffix(f, "/"+suffix) {
				if found != "" {
					return "", false // ambiguous
				}
				found = f
			}
		}
		if found != "" {
			return found, true
		}
	}
	return "", false
}

// workspaceRoot looks up the {id} workspace's repo root, writing the 404 itself
// when the workspace is unknown.
func (s *Server) workspaceRoot(w http.ResponseWriter, r *http.Request) (string, bool) {
	ws := s.mgr.Workspace(r.PathValue("id"))
	if ws == nil {
		writeError(w, http.StatusNotFound, "unknown workspace")
		return "", false
	}
	return ws.RepoPath, true
}

// writeResolveError maps resolveWorkspacePath failures to status codes:
// escapes are the caller's fault (400), everything else reads as missing (404).
func writeResolveError(w http.ResponseWriter, err error) {
	if errors.Is(err, errPathEscapes) {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeError(w, http.StatusNotFound, "no such file")
}

// getFile returns one UTF-8 text file as {"path", "content"}.
func (s *Server) getFile(w http.ResponseWriter, r *http.Request) {
	root, ok := s.workspaceRoot(w, r)
	if !ok {
		return
	}
	reqPath := r.URL.Query().Get("path")
	abs, err := resolveWorkspaceFile(root, reqPath)
	if err != nil {
		writeResolveError(w, err)
		return
	}
	fi, err := os.Stat(abs)
	if err != nil || !fi.Mode().IsRegular() {
		writeError(w, http.StatusNotFound, "no such file")
		return
	}
	if fi.Size() > maxFileBytes {
		writeError(w, http.StatusRequestEntityTooLarge, "file too large for the viewer")
		return
	}
	b, err := os.ReadFile(abs)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if !utf8.Valid(b) {
		writeError(w, http.StatusUnsupportedMediaType, "not a UTF-8 text file")
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"path": reqPath, "content": string(b)})
}

// putFile overwrites an EXISTING text file with {"path", "content"}. Creation
// is intentionally unsupported — the viewer only saves files it opened, and
// keeping the route overwrite-only means it can't be used to drop new files.
func (s *Server) putFile(w http.ResponseWriter, r *http.Request) {
	root, ok := s.workspaceRoot(w, r)
	if !ok {
		return
	}
	// Cap the body BEFORE decoding — decodeJSON buffers it all otherwise.
	// Small headroom over maxFileBytes covers the JSON envelope + escaping.
	r.Body = http.MaxBytesReader(w, r.Body, maxFileBytes*2)
	var req struct {
		Path    string `json:"path"`
		Content string `json:"content"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	if len(req.Content) > maxFileBytes {
		writeError(w, http.StatusRequestEntityTooLarge, "content too large")
		return
	}
	// Same suffix fallback as getFile, so a save targets exactly the file the
	// matching read opened.
	abs, err := resolveWorkspaceFile(root, req.Path)
	if err != nil {
		writeResolveError(w, err)
		return
	}
	fi, err := os.Stat(abs)
	if err != nil || !fi.Mode().IsRegular() {
		writeError(w, http.StatusNotFound, "no such file")
		return
	}
	if err := os.WriteFile(abs, []byte(req.Content), fi.Mode().Perm()); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

type fileEntry struct {
	Name string `json:"name"`
	Dir  bool   `json:"dir"`
}

// listDir returns a directory's entries as {"entries": [{"name", "dir"}]}.
// path "" (or omitted) lists the repo root. Hidden-file filtering is a lens
// choice, so nothing is filtered here.
func (s *Server) listDir(w http.ResponseWriter, r *http.Request) {
	root, ok := s.workspaceRoot(w, r)
	if !ok {
		return
	}
	abs, err := resolveWorkspacePath(root, r.URL.Query().Get("path"))
	if err != nil {
		writeResolveError(w, err)
		return
	}
	des, err := os.ReadDir(abs)
	if err != nil {
		writeError(w, http.StatusNotFound, "not a directory")
		return
	}
	entries := make([]fileEntry, 0, len(des))
	for _, de := range des {
		entries = append(entries, fileEntry{Name: de.Name(), Dir: entryIsDir(abs, de)})
	}
	writeJSON(w, http.StatusOK, map[string]any{"entries": entries})
}

// entryIsDir classifies a dirent, following symlinks so a linked directory
// still renders as expandable in the tree.
func entryIsDir(parent string, de fs.DirEntry) bool {
	if de.Type()&fs.ModeSymlink == 0 {
		return de.IsDir()
	}
	fi, err := os.Stat(filepath.Join(parent, de.Name()))
	return err == nil && fi.IsDir()
}
