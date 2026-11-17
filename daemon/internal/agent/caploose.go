package agent

import (
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// syncedSkillsFolder is where claude.ai puts the skills it syncs, inside
// ~/.claude/skills: they are claude.ai's, not loose skills of this machine.
const syncedSkillsFolder = "synced"

// LooseSkills lists the skill folders directly under claudeDir/skills as
// skill entries from this machine, by folder name. A folder with no
// SKILL.md, a dot folder, the synced folder and a name the catalog cannot
// hold as an id are left out.
func LooseSkills(claudeDir string) ([]Capability, error) {
	root := filepath.Join(claudeDir, skillsFolder)
	entries, err := os.ReadDir(root)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []Capability
	for _, e := range entries {
		name := e.Name()
		if !e.IsDir() || strings.HasPrefix(name, ".") || name == syncedSkillsFolder || !capabilityID.MatchString(name) {
			continue
		}
		dir := filepath.Join(root, name)
		if _, err := os.Stat(filepath.Join(dir, skillFile)); err != nil {
			continue
		}
		out = append(out, Capability{
			ID: name, Kind: CapSkill, Path: dir, Source: SourceThisMachine,
			Description: readSkill(dir).Description, Cost: skillCost(dir),
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}
