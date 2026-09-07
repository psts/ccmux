package agent

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

// Skills live in a base as skills/<name>/SKILL.md plus whatever files the
// procedure needs. They arrive from where they already live — a folder in a
// git repo (a GitHub tree URL, or any git URL and a path) or files dropped
// on the lens — never from a text box. A skill installed from git keeps
// its source in .source beside SKILL.md so it can be updated or told apart
// from a hand-written one.

// Skill is one skill as the lenses list it.
type Skill struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	// Source is the git URL it was installed from ("" = written by hand).
	Source string `json:"source,omitempty"`
	Files  int    `json:"files"`
}

const (
	skillsFolder = "skills"
	skillFile    = "SKILL.md"
	sourceFile   = ".source"
)

var skillName = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,63}$`)

// ValidSkillName reports whether name may be a folder under skills/.
func ValidSkillName(name string) bool { return skillName.MatchString(name) }

// Skills lists the base's skills, by name.
func (s *Store) Skills(agentName string) ([]Skill, error) {
	if !ValidName(agentName) {
		return nil, ErrNotFound
	}
	root := filepath.Join(s.Dir(agentName), skillsFolder)
	entries, err := os.ReadDir(root)
	if errors.Is(err, os.ErrNotExist) {
		return []Skill{}, nil
	}
	if err != nil {
		return nil, err
	}
	out := []Skill{}
	for _, e := range entries {
		if !e.IsDir() || strings.HasPrefix(e.Name(), ".") {
			continue
		}
		out = append(out, readSkill(filepath.Join(root, e.Name())))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

func readSkill(dir string) Skill {
	sk := Skill{Name: filepath.Base(dir)}
	if body, err := os.ReadFile(filepath.Join(dir, skillFile)); err == nil {
		fm := frontmatter(string(body))
		sk.Description = fm["description"]
	}
	if src, err := os.ReadFile(filepath.Join(dir, sourceFile)); err == nil {
		sk.Source = strings.TrimSpace(string(src))
	}
	filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
		if err == nil && !d.IsDir() && filepath.Base(p) != sourceFile {
			sk.Files++
		}
		return nil
	})
	return sk
}

// SkillText is a skill's SKILL.md, for a lens that shows it.
func (s *Store) SkillText(agentName, skill string) (string, error) {
	if !ValidName(agentName) || !ValidSkillName(skill) {
		return "", ErrNotFound
	}
	body, err := os.ReadFile(filepath.Join(s.Dir(agentName), skillsFolder, skill, skillFile))
	if errors.Is(err, os.ErrNotExist) {
		return "", ErrNotFound
	}
	return string(body), err
}

// frontmatter reads the YAML-ish "key: value" lines between the leading
// --- fences; enough for name and description, which is all a skill's
// header carries that a list needs.
func frontmatter(body string) map[string]string {
	out := map[string]string{}
	sc := bufio.NewScanner(strings.NewReader(body))
	if !sc.Scan() || strings.TrimSpace(sc.Text()) != "---" {
		return out
	}
	for sc.Scan() {
		line := sc.Text()
		if strings.TrimSpace(line) == "---" {
			break
		}
		if k, v, ok := strings.Cut(line, ":"); ok {
			out[strings.TrimSpace(k)] = strings.Trim(strings.TrimSpace(v), `"'`)
		}
	}
	return out
}

// PutSkillFiles installs a skill from files the lens sent (dropped on it):
// paths relative to the skill folder, text bodies. SKILL.md is required;
// the name comes from its frontmatter, else from name. An existing skill of
// that name is replaced. Bumps the base version.
func (s *Store) PutSkillFiles(agentName, name string, files map[string]string) (Skill, error) {
	if !ValidName(agentName) {
		return Skill{}, ErrNotFound
	}
	body, ok := files[skillFile]
	if !ok {
		return Skill{}, errors.New("a skill needs a SKILL.md")
	}
	name, err := skillNameFrom(body, name)
	if err != nil {
		return Skill{}, err
	}
	dir := filepath.Join(s.Dir(agentName), skillsFolder, name)
	if err := os.RemoveAll(dir); err != nil {
		return Skill{}, err
	}
	for rel, content := range files {
		clean := filepath.Clean("/" + rel)[1:] // no escape from the skill folder
		p := filepath.Join(dir, clean)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			return Skill{}, err
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			return Skill{}, err
		}
	}
	return readSkill(dir), s.noteBaseChange(agentName, "skill "+name+" added")
}

// GitSource is where a skill comes from: a repository, a ref, and the
// folder inside it.
type GitSource struct {
	Repo string
	Ref  string
	Path string
}

// ParseSkillURL reads a GitHub folder URL
// (https://github.com/o/r/tree/<ref>/<path>, or /blob/ to a SKILL.md) into
// its source. Anything else is taken as a git repository as git would (an
// https URL, an scp-style git@host:path, a local path), with an optional
// "#<ref>:<path>" or "#<path>" fragment naming the folder.
func ParseSkillURL(raw string) (GitSource, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return GitSource{}, errors.New("empty URL")
	}
	if u, err := url.Parse(raw); err == nil && u.Host == "github.com" {
		return parseGitHubURL(raw, strings.Split(strings.Trim(u.Path, "/"), "/"))
	}
	repo, frag, _ := strings.Cut(raw, "#")
	src := GitSource{Repo: repo}
	if ref, path, ok := strings.Cut(frag, ":"); ok {
		src.Ref, src.Path = ref, path
	} else {
		src.Path = frag
	}
	if strings.HasPrefix(repo, "-") {
		return GitSource{}, fmt.Errorf("repository %q: a git URL does not start with -", repo)
	}
	return src, checkSkillPath(src.Path)
}

// checkSkillPath refuses a folder that would leave the clone: absolute,
// or with a .. that climbs out. Every source path passes through here, so
// a pasted "#../../home" never reaches the copy.
func checkSkillPath(p string) error {
	if p == "" {
		return nil
	}
	clean := filepath.ToSlash(filepath.Clean(p))
	if filepath.IsAbs(p) || clean != strings.TrimSuffix(p, "/") || clean == ".." || strings.HasPrefix(clean, "../") {
		return fmt.Errorf("folder %q: must be a plain path inside the repository", p)
	}
	return nil
}

func parseGitHubURL(raw string, parts []string) (GitSource, error) {
	if len(parts) < 2 {
		return GitSource{}, fmt.Errorf("github URL %q: want /owner/repo/tree/<branch>/<folder>", raw)
	}
	src := GitSource{Repo: "https://github.com/" + parts[0] + "/" + parts[1] + ".git"}
	if len(parts) < 4 || (parts[2] != "tree" && parts[2] != "blob") {
		return src, nil
	}
	src.Ref, src.Path = parts[3], strings.Join(parts[4:], "/")
	if parts[2] == "blob" {
		src.Path = filepath.Dir(src.Path)
	}
	return src, checkSkillPath(src.Path)
}

// InstallSkillFromGit fetches src.Path from src.Repo at src.Ref (the
// remote's default branch when empty) with a shallow sparse clone and
// installs it as a skill, named after the folder unless SKILL.md says
// otherwise. source is what .source records, for Update. Replaces an
// existing skill of that name. Bumps the base version.
func (s *Store) InstallSkillFromGit(ctx context.Context, agentName string, src GitSource, source string) (Skill, error) {
	if !ValidName(agentName) {
		return Skill{}, ErrNotFound
	}
	if err := checkSkillPath(src.Path); err != nil {
		return Skill{}, err
	}
	tmp, err := os.MkdirTemp("", "ccmux-skill-")
	if err != nil {
		return Skill{}, err
	}
	defer os.RemoveAll(tmp)
	from, err := fetchSkillFolder(ctx, tmp, src)
	if err != nil {
		return Skill{}, err
	}
	name, err := skillNameOf(from)
	if err != nil {
		return Skill{}, fmt.Errorf("%s %q: %w", src.Repo, src.Path, err)
	}
	return s.installSkillDir(agentName, name, from, source)
}

// fetchSkillFolder clones src into tmp and returns the folder to install,
// refusing one that resolves outside the clone.
func fetchSkillFolder(ctx context.Context, tmp string, src GitSource) (string, error) {
	if err := sparseClone(ctx, tmp, src); err != nil {
		return "", err
	}
	from := filepath.Join(tmp, filepath.FromSlash(src.Path))
	if rel, err := filepath.Rel(tmp, from); err != nil || rel == ".." || strings.HasPrefix(rel, "../") {
		return "", fmt.Errorf("folder %q leaves the clone", src.Path)
	}
	return from, nil
}

// installSkillDir replaces the agent's skill name with the folder at from,
// records its source and bumps the base.
func (s *Store) installSkillDir(agentName, name, from, source string) (Skill, error) {
	dir := filepath.Join(s.Dir(agentName), skillsFolder, name)
	if err := os.RemoveAll(dir); err != nil {
		return Skill{}, err
	}
	if err := copyTree(from, dir); err != nil {
		return Skill{}, err
	}
	if err := os.WriteFile(filepath.Join(dir, sourceFile), []byte(source+"\n"), 0o644); err != nil {
		return Skill{}, err
	}
	return readSkill(dir), s.noteBaseChange(agentName, "skill "+name+" from "+source)
}

// skillNameOf is the name a fetched folder installs under: its SKILL.md
// frontmatter name, else the folder's own name; checked either way.
func skillNameOf(dir string) (string, error) {
	body, err := os.ReadFile(filepath.Join(dir, skillFile))
	if err != nil {
		return "", errors.New("no " + skillFile + " there")
	}
	return skillNameFrom(string(body), filepath.Base(dir))
}

// skillNameFrom is the one rule for a skill's name, whether it came from
// git or from dropped files: the SKILL.md frontmatter name wins over the
// fallback, and either must be a valid folder name.
func skillNameFrom(body, fallback string) (string, error) {
	name := fallback
	if fm := frontmatter(body)["name"]; fm != "" {
		name = fm
	}
	if !ValidSkillName(name) {
		return "", fmt.Errorf("skill name %q: use a-z, 0-9, - and _", name)
	}
	return name, nil
}

// UpdateSkill reinstalls a skill from its recorded source.
func (s *Store) UpdateSkill(ctx context.Context, agentName, skill string) (Skill, error) {
	if !ValidName(agentName) || !ValidSkillName(skill) {
		return Skill{}, ErrNotFound
	}
	raw, err := os.ReadFile(filepath.Join(s.Dir(agentName), skillsFolder, skill, sourceFile))
	if err != nil {
		return Skill{}, fmt.Errorf("skill %s was not installed from a URL; nothing to update from", skill)
	}
	src, err := ParseSkillURL(strings.TrimSpace(string(raw)))
	if err != nil {
		return Skill{}, err
	}
	return s.InstallSkillFromGit(ctx, agentName, src, strings.TrimSpace(string(raw)))
}

// DeleteSkill removes one skill folder. Bumps the base version.
func (s *Store) DeleteSkill(agentName, skill string) error {
	if !ValidName(agentName) || !ValidSkillName(skill) {
		return ErrNotFound
	}
	dir := filepath.Join(s.Dir(agentName), skillsFolder, skill)
	if _, err := os.Stat(dir); err != nil {
		return ErrNotFound
	}
	if err := os.RemoveAll(dir); err != nil {
		return err
	}
	return s.noteBaseChange(agentName, "skill "+skill+" removed")
}

// noteBaseChange bumps the base version and writes the CHANGELOG line for a
// change outside agent.json and AGENTS.md, so instances see drift.
func (s *Store) noteBaseChange(agentName, note string) error {
	dir := s.Dir(agentName)
	if _, err := s.bumpIfChanged(dir, true); err != nil {
		return err
	}
	return appendFile(filepath.Join(dir, fileChangelog), "  "+note+"\n")
}

func sparseClone(ctx context.Context, into string, src GitSource) error {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	run := func(dir string, args ...string) error {
		cmd := exec.CommandContext(ctx, "git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
		if out, err := cmd.CombinedOutput(); err != nil {
			return fmt.Errorf("git %s (%s): %v: %s", args[0], src.Repo, err, strings.TrimSpace(string(out)))
		}
		return nil
	}
	clone := []string{"clone", "--depth", "1", "--no-checkout", "--quiet"}
	if src.Ref != "" {
		clone = append(clone, "--branch", src.Ref)
	}
	// "--" ends the options: a repository or ref that starts with - is an
	// argument, never a flag.
	if err := run("", append(clone, "--", src.Repo, into)...); err != nil {
		return err
	}
	if src.Path != "" {
		if err := run(into, "sparse-checkout", "set", "--no-cone", "--", src.Path); err != nil {
			return err
		}
	}
	return run(into, "checkout", "--quiet")
}

func copyTree(from, to string) error {
	return filepath.WalkDir(from, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(from, p)
		target := filepath.Join(to, rel)
		if d.Type()&os.ModeSymlink != 0 {
			// A link in a fetched repo points wherever its author chose,
			// a private key included; the skill does without it.
			return nil
		}
		if d.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		return os.WriteFile(target, b, 0o644)
	})
}
