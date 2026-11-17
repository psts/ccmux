package loadout

import (
	"fmt"
	"log"
	"sort"

	"ccmux.dev/ccmuxd/internal/agent"
	"ccmux.dev/ccmuxd/internal/store"
)

// The day-one choices. Before loadouts, every thread session loaded the
// whole of the user's Claude setup. Starting every group empty would switch
// the commit gates (the qa and solo-workflow plugins) off in every thread
// session at once, so the first start of a daemon that has loadouts writes
// each group the choices its repos live by today:
//
//   - the plugins its repos' own settings switch on (.claude/settings.json
//     and .claude/settings.local.json of each repo in the group);
//   - chrome-pool;
//   - the loose skills in ~/.claude/skills.
//
// Connectors and everything else stay off. A repo in no group gets the
// same as its own choices, since it has no group to carry them. Groups and
// repos made after this start with nothing, as designed.
//
// It is written once (store.SeedCapabilityChoices keeps the mark), and it
// only reads files: no session is started and nothing is fetched.

// seedPlugin is the plugin every group starts with besides its repos' own.
const seedPlugin = "chrome-pool"

// Seed writes the day-one choices if they were never written.
func (s *Service) Seed() error {
	s.seeding.Lock()
	defer s.seeding.Unlock()
	seeded, err := s.St.CapabilityChoicesSeeded()
	if err != nil || seeded {
		return err
	}
	claudeDir, err := s.ClaudeDir()
	if err != nil {
		return err
	}
	// The catalog may be empty: fill it from what is installed, so there
	// is an entry to switch on for every plugin a repo uses.
	imported, err := s.Catalog.ImportCapabilities(claudeDir, nil)
	if err != nil {
		return fmt.Errorf("filling the catalog: %w", err)
	}
	for _, why := range imported.Skipped {
		log.Printf("skills and plugins: day-one choices: the catalog left out %s", why)
	}
	installed, err := agent.ReadInstalledPlugins(agent.InstalledPluginsPath(claudeDir))
	if err != nil {
		return fmt.Errorf("reading the installed plugins: %w", err)
	}
	rows, err := s.seedRows(installed)
	if err != nil {
		return err
	}
	if _, err := s.St.SeedCapabilityChoices(rows, s.Now().UnixMilli()); err != nil {
		return err
	}
	log.Printf("skills and plugins: day-one choices written (%d rows); groups and repos made from now on start with nothing on", len(rows))
	return nil
}

// seedRows works the day-one choices out. installed is Claude Code's
// installed plugins, so a plugin left out can say why.
func (s *Service) seedRows(installed map[string]string) ([]store.CapChoice, error) {
	catalog, err := s.Catalog.Capabilities()
	if err != nil {
		return nil, err
	}
	sources, err := s.Catalog.Sources()
	if err != nil {
		return nil, err
	}
	windows, err := s.St.AllWindows()
	if err != nil {
		return nil, err
	}
	members, err := s.St.WindowMembers()
	if err != nil {
		return nil, err
	}
	base := seedBase(catalog)
	set := seedSet{}
	for id := range windows {
		set.add(store.ChoiceWindow, id, base)
	}
	for _, repo := range s.Repos() {
		scope, id := store.ChoiceWorkspace, repo.WorkspaceID
		if _, ok := windows[members[repo.WorkspaceID]]; ok {
			scope, id = store.ChoiceWindow, members[repo.WorkspaceID]
		}
		set.add(scope, id, append(repoPlugins(repo, catalog, sources, installed), base...))
	}
	return set.rows(), nil
}

// seedSet is the rows to write, each once.
type seedSet map[store.CapChoice]bool

func (set seedSet) add(scope, scopeID string, capIDs []string) {
	for _, capID := range capIDs {
		set[store.CapChoice{Scope: scope, ScopeID: scopeID, CapID: capID, State: store.ChoiceOn}] = true
	}
}

// rows lists the set in a fixed order.
func (set seedSet) rows() []store.CapChoice {
	out := make([]store.CapChoice, 0, len(set))
	for c := range set {
		out = append(out, c)
	}
	sort.Slice(out, func(i, j int) bool {
		a, b := out[i], out[j]
		return a.Scope+"/"+a.ScopeID+"/"+a.CapID < b.Scope+"/"+b.ScopeID+"/"+b.CapID
	})
	return out
}

// seedBase is what every group starts with: chrome-pool and the loose
// skills of this machine, where the catalog holds them.
func seedBase(catalog []agent.Capability) []string {
	var out []string
	for _, c := range catalog {
		switch {
		case c.Kind == agent.CapPlugin && c.ID == seedPlugin:
			out = append(out, c.ID)
		case c.Kind == agent.CapSkill && c.Source == agent.SourceThisMachine:
			out = append(out, c.ID)
		}
	}
	return out
}

// repoPlugins are the catalog entries of the plugins a repo's own settings
// switch on. A plugin the catalog has no entry for and a settings file that
// does not parse are logged and left out. The log says which of two causes
// it is: never installed (Claude Code loads nothing either), or installed
// but its name taken by another catalog entry (the repo used it, so it must
// be turned on by hand; the seed runs once).
func repoPlugins(repo Repo, catalog []agent.Capability, sources []agent.CapSource, installed map[string]string) []string {
	enabled, err := agent.EnabledPlugins(repo.Path)
	if err != nil {
		log.Printf("skills and plugins: day-one choices: %s: %v", repo.Path, err)
	}
	var out []string
	for _, pluginID := range enabled {
		id, ok := agent.CatalogIDOfPlugin(catalog, sources, pluginID)
		if !ok {
			logLeftOut(repo.Path, pluginID, installed)
			continue
		}
		out = append(out, id)
	}
	return out
}

// logLeftOut says why a plugin a repo switches on got no day-one choice.
func logLeftOut(repoPath, pluginID string, installed map[string]string) {
	if _, ok := installed[pluginID]; ok {
		log.Printf("skills and plugins: day-one choices: %s switches on %s, which is installed but has no catalog entry (its name is taken by another entry); left out, turn it on by hand", repoPath, pluginID)
		return
	}
	log.Printf("skills and plugins: day-one choices: %s switches on %s, which is not installed on this host; left out", repoPath, pluginID)
}
