package api

import (
	"encoding/json"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// callProjects hits listProjects directly (it touches only projectsRoot, no
// manager/tmux needed) and returns the recorder. rawQuery is e.g. "path=SaaC".
func callProjects(t *testing.T, root string, rawQuery ...string) *httptest.ResponseRecorder {
	t.Helper()
	s := &Server{projectsRoot: root}
	rec := httptest.NewRecorder()
	url := "/v1/projects"
	if len(rawQuery) > 0 {
		url += "?" + rawQuery[0]
	}
	s.listProjects(rec, httptest.NewRequest("GET", url, nil))
	return rec
}

// projectsResp decodes the listProjects body for assertions.
type projectsResp struct {
	Root     string  `json:"root"`
	Path     string  `json:"path"`
	Parent   *string `json:"parent"`
	Projects []struct {
		Name string `json:"name"`
		Path string `json:"path"`
		Git  bool   `json:"git"`
	} `json:"projects"`
}

func decodeProjects(t *testing.T, rec *httptest.ResponseRecorder) projectsResp {
	t.Helper()
	var got projectsResp
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v (body %s)", err, rec.Body)
	}
	return got
}

func TestListProjects(t *testing.T) {
	root := t.TempDir()
	// alpha: a git repo; beta: plain dir; .hidden + a file: skipped;
	// gamma-link: symlink to a directory (followed).
	for _, d := range []string{"alpha/.git", "beta", ".hidden"} {
		if err := os.MkdirAll(filepath.Join(root, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(root, "notes.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	target := t.TempDir()
	if err := os.Symlink(target, filepath.Join(root, "gamma-link")); err != nil {
		t.Fatal(err)
	}

	rec := callProjects(t, root)
	if rec.Code != 200 {
		t.Fatalf("status = %d, want 200 (body %s)", rec.Code, rec.Body)
	}
	got := decodeProjects(t, rec)
	if got.Root != root {
		t.Errorf("root = %q, want %q", got.Root, root)
	}
	if got.Path != "" || got.Parent != nil {
		t.Errorf("at root: path = %q, parent = %v, want \"\" and absent", got.Path, got.Parent)
	}
	if len(got.Projects) != 3 {
		t.Fatalf("projects = %+v, want alpha/beta/gamma-link", got.Projects)
	}
	for i, want := range []struct {
		name string
		git  bool
	}{{"alpha", true}, {"beta", false}, {"gamma-link", false}} {
		p := got.Projects[i]
		if p.Name != want.name || p.Git != want.git {
			t.Errorf("projects[%d] = %+v, want name=%s git=%v", i, p, want.name, want.git)
		}
		if p.Path != filepath.Join(root, p.Name) {
			t.Errorf("projects[%d].path = %q, want under root", i, p.Path)
		}
	}
}

func TestListProjects_Subpath(t *testing.T) {
	root := t.TempDir()
	// root/group/inner is the project two levels down.
	if err := os.MkdirAll(filepath.Join(root, "group", "inner", ".git"), 0o755); err != nil {
		t.Fatal(err)
	}

	rec := callProjects(t, root, "path=group")
	if rec.Code != 200 {
		t.Fatalf("status = %d, want 200 (body %s)", rec.Code, rec.Body)
	}
	got := decodeProjects(t, rec)
	if got.Path != "group" {
		t.Errorf("path = %q, want group", got.Path)
	}
	if got.Parent == nil || *got.Parent != "" {
		t.Errorf("parent = %v, want \"\" (the root)", got.Parent)
	}
	if len(got.Projects) != 1 || got.Projects[0].Name != "inner" || !got.Projects[0].Git {
		t.Fatalf("projects = %+v, want inner (git)", got.Projects)
	}
	if got.Projects[0].Path != filepath.Join(root, "group", "inner") {
		t.Errorf("inner path = %q, want under root/group", got.Projects[0].Path)
	}

	// One level deeper: parent points back to "group".
	rec = callProjects(t, root, "path=group%2Finner")
	got = decodeProjects(t, rec)
	if got.Path != "group/inner" || got.Parent == nil || *got.Parent != "group" {
		t.Errorf("path = %q, parent = %v, want group/inner with parent group", got.Path, got.Parent)
	}
}

func TestListProjects_TraversalRejected(t *testing.T) {
	root := t.TempDir()
	for _, q := range []string{
		"path=..",
		"path=../..",
		"path=a/../..",
		"path=%2Fetc", // absolute
	} {
		if rec := callProjects(t, root, q); rec.Code != 400 {
			t.Errorf("%s: status = %d, want 400 (body %s)", q, rec.Code, rec.Body)
		}
	}
	// "a/.." cleans to the root itself — legal, not an escape.
	if rec := callProjects(t, root, "path=a%2F.."); rec.Code != 200 {
		t.Errorf("a/..: status = %d, want 200 (body %s)", rec.Code, rec.Body)
	}
}

func TestListProjects_GitFileWorktree(t *testing.T) {
	// A linked git worktree has a .git *file*, not a directory — still a repo.
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "wt"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "wt", ".git"), []byte("gitdir: /elsewhere"), 0o644); err != nil {
		t.Fatal(err)
	}
	rec := callProjects(t, root)
	var got struct {
		Projects []struct {
			Git bool `json:"git"`
		} `json:"projects"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(got.Projects) != 1 || !got.Projects[0].Git {
		t.Fatalf("projects = %+v, want one entry with git=true", got.Projects)
	}
}

func TestListProjects_EmptyRoot(t *testing.T) {
	rec := callProjects(t, t.TempDir())
	if rec.Code != 200 {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	// An empty root must serialize as [], not null — lenses index into it.
	if body := rec.Body.String(); !json.Valid([]byte(body)) || !containsProjectsArray(body) {
		t.Fatalf("body = %s, want projects: []", body)
	}
}

func containsProjectsArray(body string) bool {
	var got map[string]json.RawMessage
	if json.Unmarshal([]byte(body), &got) != nil {
		return false
	}
	return string(got["projects"]) == "[]"
}

func TestListProjects_Unconfigured(t *testing.T) {
	if rec := callProjects(t, ""); rec.Code != 503 {
		t.Fatalf("status = %d, want 503", rec.Code)
	}
}

func TestListProjects_MissingRoot(t *testing.T) {
	if rec := callProjects(t, filepath.Join(t.TempDir(), "nope")); rec.Code != 500 {
		t.Fatalf("status = %d, want 500", rec.Code)
	}
}

// callCreateProject hits createProject directly with a JSON body.
func callCreateProject(t *testing.T, root, body string) *httptest.ResponseRecorder {
	t.Helper()
	s := &Server{projectsRoot: root}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/v1/projects", strings.NewReader(body))
	s.createProject(rec, req)
	return rec
}

func TestCreateProject_Plain(t *testing.T) {
	root := t.TempDir()
	rec := callCreateProject(t, root, `{"path":"chartlabs"}`)
	if rec.Code != 201 {
		t.Fatalf("status = %d, want 201 (body %s)", rec.Code, rec.Body)
	}
	fi, err := os.Stat(filepath.Join(root, "chartlabs"))
	if err != nil || !fi.IsDir() {
		t.Fatalf("folder not created: %v", err)
	}
	var got struct {
		Name string `json:"name"`
		Path string `json:"path"`
		Git  bool   `json:"git"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Name != "chartlabs" || got.Path != filepath.Join(root, "chartlabs") || got.Git {
		t.Errorf("entry = %+v, want chartlabs, abs path, git=false", got)
	}
}

func TestCreateProject_GitInit(t *testing.T) {
	root := t.TempDir()
	rec := callCreateProject(t, root, `{"path":"backend","git":true}`)
	if rec.Code != 201 {
		t.Fatalf("status = %d, want 201 (body %s)", rec.Code, rec.Body)
	}
	if _, err := os.Stat(filepath.Join(root, "backend", ".git")); err != nil {
		t.Fatalf(".git missing after git:true: %v", err)
	}
	// The list must now show it as a git folder.
	got := decodeProjects(t, callProjects(t, root))
	if len(got.Projects) != 1 || !got.Projects[0].Git {
		t.Errorf("projects = %+v, want one git entry", got.Projects)
	}
}

// A failing git init must name its own cause and leave nothing behind. The
// no-output failures are the likely ones on a fresh host, so an empty PATH
// stands in for "git is not installed here".
func TestCreateProject_GitInitFailure(t *testing.T) {
	t.Setenv("PATH", "")
	root := t.TempDir()
	rec := callCreateProject(t, root, `{"path":"backend","git":true}`)
	if rec.Code != 500 {
		t.Fatalf("status = %d, want 500 (body %s)", rec.Code, rec.Body)
	}
	if msg := rec.Body.String(); !strings.Contains(msg, "git") || strings.Contains(msg, `"git init: "`) {
		t.Errorf("error body = %s, want the underlying cause, not a bare prefix", msg)
	}
	// Rolled back: the name must be free for a retry, not 409 forever.
	if _, err := os.Stat(filepath.Join(root, "backend")); !os.IsNotExist(err) {
		t.Errorf("folder still present after a failed git init (stat err = %v)", err)
	}
}

func TestCreateProject_Nested(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "chartlabs"), 0o755); err != nil {
		t.Fatal(err)
	}
	if rec := callCreateProject(t, root, `{"path":"chartlabs/backend"}`); rec.Code != 201 {
		t.Fatalf("status = %d, want 201 (body %s)", rec.Code, rec.Body)
	}
	// Missing parent: Mkdir refuses rather than building the tree.
	if rec := callCreateProject(t, root, `{"path":"nope/deep"}`); rec.Code != 500 {
		t.Errorf("missing parent: status = %d, want 500", rec.Code)
	}
}

func TestCreateProject_Rejections(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "taken"), 0o755); err != nil {
		t.Fatal(err)
	}
	for body, want := range map[string]int{
		`{"path":"taken"}`:    409, // already exists
		`{"path":""}`:         400, // no name
		`{"path":"../out"}`:   400, // escapes the root
		`{"path":"/etc/x"}`:   400, // absolute
		`{"path":"a/.."}`:     400, // cleans to the root itself
		`{"path":".hidden"}`:  400, // listProjects would never show it
		`{"path":"sub/.git"}`: 400, // dot-name nested
		`not json`:            400,
	} {
		if rec := callCreateProject(t, root, body); rec.Code != want {
			t.Errorf("%s: status = %d, want %d (body %s)", body, rec.Code, want, rec.Body)
		}
	}
}

func TestCreateProject_Unconfigured(t *testing.T) {
	if rec := callCreateProject(t, "", `{"path":"x"}`); rec.Code != 503 {
		t.Fatalf("status = %d, want 503", rec.Code)
	}
}
