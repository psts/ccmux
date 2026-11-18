package todos

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
)

// Keys names the list a folder's items go on: its repo's main folder. A
// task builder or a thread's helper works in a git worktree of the repo,
// somewhere else on disk, and what it finds belongs on the repo's own
// list, the one the repo's row on the thread page shows. Every path that
// names a repo goes through here (a workspace's, a page's, a tool's), so
// two spellings of one repo (a worktree, a subfolder, a symlink) are never
// two lists. A folder that is in no git repo is its own list.
type Keys struct {
	// Git runs git in dir and returns what it printed, with what it wrote
	// to stderr in the error when it fails; nil runs the real one.
	Git func(dir string, args ...string) (string, error)
	// Exists says whether a folder is on this host; nil asks the disk.
	Exists func(dir string) bool

	mu    sync.Mutex
	known map[string]string
}

// Of is the key of the list for path: the main folder of the git repo that
// holds it, else the path itself, cleaned, for a folder that is in no repo
// or is not on this host. What git answered is kept: a folder does not
// move to another repo, and the page asks for every repo on every read.
//
// A git that could not answer (not installed, a repo it refuses to read)
// is an error, never a key: an item filed under the worktree's own path
// would be on a list no page shows, and gone from sight with the worktree.
func (k *Keys) Of(path string) (string, error) {
	if path == "" {
		return "", nil
	}
	path = filepath.Clean(path)
	k.mu.Lock()
	key, ok := k.known[path]
	k.mu.Unlock()
	if ok {
		return key, nil
	}
	if !k.exists(path) {
		return path, nil // never kept: it may be made, and be a repo, later
	}
	root, err := k.mainRoot(path)
	if err != nil {
		return "", fmt.Errorf("which repo %s is in could not be read: %w", path, err)
	}
	if root == "" {
		return path, nil // in no repo; not kept either, git init may run there
	}
	k.mu.Lock()
	if k.known == nil {
		k.known = map[string]string{}
	}
	k.known[path] = root
	k.mu.Unlock()
	return root, nil
}

// notARepo is how git says a folder is in no repository, which is an
// answer; anything else it fails with is not.
const notARepo = "not a git repository"

// mainRoot is the main folder of the repo holding dir, "" when dir is in
// none. Every worktree of a repo shares one git folder (its "common dir"),
// and the folder that holds it is the repo's own. A repo whose git folder
// is not a ".git" inside it (a submodule, a bare repo) has no such folder
// to name, so there the work tree's top stands in.
func (k *Keys) mainRoot(dir string) (string, error) {
	common, err := k.git(dir, "rev-parse", "--path-format=absolute", "--git-common-dir")
	if err != nil {
		if strings.Contains(err.Error(), notARepo) {
			return "", nil
		}
		return "", err
	}
	if filepath.Base(common) == ".git" {
		return filepath.Dir(common), nil
	}
	return k.git(dir, "rev-parse", "--show-toplevel")
}

func (k *Keys) exists(dir string) bool {
	if k.Exists != nil {
		return k.Exists(dir)
	}
	fi, err := os.Stat(dir)
	return err == nil && fi.IsDir()
}

func (k *Keys) git(dir string, args ...string) (string, error) {
	if k.Git != nil {
		return k.Git(dir, args...)
	}
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	cmd.Env = append(os.Environ(), "LC_ALL=C") // notARepo is read in git's own words, not a translation's
	out, err := cmd.Output()
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		return "", fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(string(exit.Stderr)))
	}
	if err != nil {
		return "", fmt.Errorf("git %s: %w", strings.Join(args, " "), err)
	}
	return strings.TrimSpace(string(out)), nil
}
