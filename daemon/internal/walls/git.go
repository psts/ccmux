package walls

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Git is where a folder's git data lives. For a repo's own folder both are
// its .git; for a worktree, Dir is the worktree's own entry
// (<repo>/.git/worktrees/<name>) and Common the repo's .git, which every
// worktree of the repo shares.
type Git struct{ Dir, Common string }

// GitOf reads where folder's git data lives from the files git itself
// follows (the .git file of a worktree and its commondir), without running
// git: the answer names what gets walled, so it must not depend on a
// config the walled work could have changed.
func GitOf(folder string) (Git, error) {
	dot := filepath.Join(folder, ".git")
	fi, err := os.Stat(dot)
	if err != nil {
		return Git{}, fmt.Errorf("%s is not a git folder: %w", folder, err)
	}
	if fi.IsDir() {
		return Git{Dir: dot, Common: dot}, nil
	}
	dir, err := pointer(dot, "gitdir: ", folder)
	if err != nil {
		return Git{}, err
	}
	common, err := pointer(filepath.Join(dir, "commondir"), "", dir)
	if os.IsNotExist(err) {
		return Git{Dir: dir, Common: dir}, nil
	}
	if err != nil {
		return Git{}, err
	}
	return Git{Dir: dir, Common: common}, nil
}

// pointer reads a one-line file that names a folder, after prefix, and
// resolves a relative name against base.
func pointer(file, prefix, base string) (string, error) {
	b, err := os.ReadFile(file)
	if err != nil {
		return "", err
	}
	line, ok := strings.CutPrefix(strings.TrimSpace(string(b)), prefix)
	if !ok || line == "" {
		return "", fmt.Errorf("%s does not name a git folder", file)
	}
	if !filepath.IsAbs(line) {
		line = filepath.Join(base, line)
	}
	return filepath.Clean(line), nil
}

// WorktreeWalls opens a worktree for the session that works in it: the
// worktree, and of the repo's .git only what a commit, a rebase and a
// stash write (the objects, the refs, their logs and the worktree's own
// entry). The rest of .git stays read-only, and with it everything that
// would run later outside the walls: the hooks, and the config (hooksPath,
// fsmonitor and filter commands live there). The three files that say
// where the worktree's git data is are sealed too: repointed, they would
// lead the next git command in the worktree (the daemon's own) to a config
// of the session's making.
//
// packed-refs is not opened: git replaces that file through a rename in
// .git itself, never in place, so only all of .git would do. What needs it
// (deleting a packed branch, a gc) is not a session's work.
func (p *Plan) WorktreeWalls(worktree string) error {
	git, err := GitOf(worktree)
	if err != nil {
		return err
	}
	if git.Dir == git.Common {
		return fmt.Errorf("%s is a repo's own folder, not a worktree: a session is walled into a worktree only", worktree)
	}
	p.Write = append(p.Write, worktree, git.Dir, filepath.Join(git.Common, "objects"), filepath.Join(git.Common, "refs"))
	p.Caches = append(p.Caches, filepath.Join(git.Common, "logs"))
	p.Sealed = append(p.Sealed, existing(filepath.Join(worktree, ".git"), filepath.Join(git.Dir, "commondir"), filepath.Join(git.Dir, "gitdir"))...)
	p.Outside = append(p.Outside, existing(filepath.Join(git.Common, "hooks"), filepath.Join(git.Common, "config"))...)
	return nil
}

// RepoWalls opens a repo's own folder for a job that lands work in it (a
// fast-forward rewrites its files, and removing a merged worktree rewrites
// .git), with the hooks and the config sealed. The repo's .git is named as
// a folder of its own: that makes it a mount of its own, which cannot be
// renamed out of the way to put another .git in its place.
func (p *Plan) RepoWalls(repo string) error {
	git, err := GitOf(repo)
	if err != nil {
		return err
	}
	hooks := filepath.Join(git.Common, "hooks")
	if err := os.MkdirAll(hooks, 0o755); err != nil {
		return fmt.Errorf("%s has no hooks folder to seal, and none could be made: %w", repo, err)
	}
	p.Write = append(p.Write, repo, git.Common)
	p.Sealed = append(p.Sealed, hooks)
	p.Sealed = append(p.Sealed, existing(filepath.Join(git.Common, "config"))...)
	return nil
}

// existing keeps the paths that are there: systemd fails a unit on a
// sealed path that is missing, and the check cannot try a write at one.
func existing(paths ...string) []string {
	var out []string
	for _, path := range paths {
		if _, err := os.Stat(path); err == nil {
			out = append(out, path)
		}
	}
	return out
}

// Existing is the folders of paths that are there, for a list systemd must
// find whole (Plan.Write).
func Existing(paths []string) []string { return existing(paths...) }

// Caches are the folders under home a check writes besides its own work,
// on any host that has them. Traced on a real run of this repo's checks
// behind walls (2026-11-19, strace, every write refused as read-only): the
// Go build and module caches and ~/.cache, with npm's cache beside them.
// What was still refused and is left closed, since nothing failed on it:
// Go's telemetry counters (~/.config/go), the shell's history, and a hook
// trace under ~/Library. ~/go/bin is not in the list: it is on the PATH,
// so what lands there runs later, outside the walls.
func Caches(home string) []string {
	return []string{
		filepath.Join(home, ".cache"),
		filepath.Join(home, "go", "pkg", "mod"),
		filepath.Join(home, ".npm"),
	}
}
