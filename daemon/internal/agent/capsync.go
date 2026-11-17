package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"
)

// Updating a source: fetch and fast-forward ccmux's checkout of it, read
// its marketplace.json, and make every plugin it holds a catalog entry
// pointing at the plugin's folder in the checkout. A pinned entry stays
// where it is (PinCapability) and is reported behind instead.

// VersionChange is one entry whose version moved, or (Behind) would have.
type VersionChange struct {
	ID   string `json:"id"`
	From string `json:"from"`
	To   string `json:"to"`
}

// SourceUpdate is what one update did. External names the plugins the
// marketplace lists from somewhere else (another repo, a url): they are
// not in this checkout, so they get no entry. Gone names entries of this
// source its marketplace no longer lists; they stay until removed by hand.
type SourceUpdate struct {
	Source   CapSource       `json:"source"`
	From     string          `json:"from"`
	Added    []string        `json:"added"`
	Changed  []VersionChange `json:"changed"`
	Behind   []VersionChange `json:"behind"`
	Gone     []string        `json:"gone"`
	Skipped  []string        `json:"skipped"`
	External []string        `json:"external"`
}

// sourcePlugin is one plugin a checkout holds, with what its entry says.
type sourcePlugin struct {
	name, rel, description, version string
	cost                            int
}

// fromCheckout says whether an entry points into a source's checkout:
// only the sync sets Commit on an entry.
func fromCheckout(c Capability) bool { return c.Commit != "" && c.Source != "" }

// UpdateSource brings source id's checkout up to date (cloning it the
// first time) and syncs its plugins into the catalog.
func (s *Store) UpdateSource(ctx context.Context, id string) (SourceUpdate, error) {
	gitMu.Lock()
	defer gitMu.Unlock()
	src, err := s.source(id)
	if err != nil {
		return SourceUpdate{}, err
	}
	dir := s.checkoutDir(id)
	commit, err := syncCheckout(ctx, src.URL, dir)
	if err != nil {
		return SourceUpdate{}, fmt.Errorf("source %s: %w", id, err)
	}
	market, plugins, external, err := readSourcePlugins(ctx, dir)
	if err != nil {
		return SourceUpdate{}, fmt.Errorf("source %s: %w", id, err)
	}
	res := SourceUpdate{From: src.Commit, Added: []string{}, Changed: []VersionChange{}, Behind: []VersionChange{}, Gone: []string{}, Skipped: []string{}, External: orEmpty(external)}
	src.Marketplace, src.Commit, src.Updated = market, commit, time.Now().UTC().Format(time.RFC3339)
	src.Plugins = map[string]string{}
	for _, p := range plugins {
		src.Plugins[p.name] = p.version
	}
	capMu.Lock()
	defer capMu.Unlock()
	if err := s.mergeSource(src, plugins, &res); err != nil {
		return SourceUpdate{}, err
	}
	res.Source = src
	return res, s.saveSourceRecord(src)
}

// source is the record of id, ErrNotFound when there is none.
func (s *Store) source(id string) (CapSource, error) {
	capMu.Lock()
	defer capMu.Unlock()
	list, err := s.Sources()
	if err != nil {
		return CapSource{}, err
	}
	i := slices.IndexFunc(list, func(e CapSource) bool { return e.ID == id })
	if i < 0 {
		return CapSource{}, ErrNotFound
	}
	return list[i], nil
}

// saveSourceRecord replaces src's record; the caller holds capMu. A source
// removed while it was fetching stays removed.
func (s *Store) saveSourceRecord(src CapSource) error {
	list, err := s.Sources()
	if err != nil {
		return err
	}
	i := slices.IndexFunc(list, func(e CapSource) bool { return e.ID == src.ID })
	if i < 0 {
		return ErrNotFound
	}
	list[i] = src
	return s.writeSources(list)
}

// marketEntry is one plugin in a marketplace.json. Source is a folder
// ("./plugins/qa") or an object naming somewhere else.
type marketEntry struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Version     string          `json:"version"`
	Source      json.RawMessage `json:"source"`
}

// readSourcePlugins reads a checkout's marketplace: its name, the plugins
// whose folder is inside the checkout, and the names of the others.
func readSourcePlugins(ctx context.Context, dir string) (string, []sourcePlugin, []string, error) {
	var doc struct {
		Name    string        `json:"name"`
		Plugins []marketEntry `json:"plugins"`
	}
	raw, err := os.ReadFile(filepath.Join(dir, ".claude-plugin", "marketplace.json"))
	if err != nil {
		return "", nil, nil, err
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		return "", nil, nil, fmt.Errorf("marketplace.json: %w", err)
	}
	var local []sourcePlugin
	var external []string
	for _, m := range doc.Plugins {
		rel, ok := localFolder(dir, m.Source)
		if !ok {
			external = append(external, m.Name)
			continue
		}
		local = append(local, readSourcePlugin(ctx, dir, rel, m))
	}
	return doc.Name, local, external, nil
}

// localFolder is a marketplace source's folder relative to the checkout,
// when it is a folder that exists inside it.
func localFolder(dir string, source json.RawMessage) (string, bool) {
	var rel string
	if json.Unmarshal(source, &rel) != nil || strings.Contains(rel, "://") {
		return "", false
	}
	rel = filepath.Clean(rel)
	if !filepath.IsLocal(rel) {
		return "", false
	}
	st, err := os.Stat(filepath.Join(dir, rel))
	return rel, err == nil && st.IsDir()
}

// readSourcePlugin is one local plugin's entry data. Its version is its
// manifest's, else the marketplace's, else the last commit on its folder.
func readSourcePlugin(ctx context.Context, dir, rel string, m marketEntry) sourcePlugin {
	abs := filepath.Join(dir, rel)
	v := manifestVersion(abs)
	if v == "" {
		v = strings.TrimSpace(m.Version)
	}
	if v == "" {
		v = folderCommit(ctx, dir, rel)
	}
	return sourcePlugin{name: m.Name, rel: rel, description: m.Description, version: v, cost: pluginCost(abs, -1)}
}

// mergeSource syncs src's plugins into the catalog; the caller holds capMu.
func (s *Store) mergeSource(src CapSource, plugins []sourcePlugin, res *SourceUpdate) error {
	catalog, err := s.Capabilities()
	if err != nil {
		return err
	}
	listed := map[string]bool{}
	for _, p := range plugins {
		listed[p.name] = true
		catalog = s.mergePlugin(catalog, src, p, res)
	}
	for _, c := range catalog {
		if c.Source == src.ID && fromCheckout(c) && !listed[c.ID] {
			res.Gone = append(res.Gone, c.ID)
		}
	}
	return s.writeCapabilities(catalog)
}

// mergePlugin adds or refreshes one plugin's entry. An entry of this
// source moves to the new version unless pinned. An imported entry of the
// same plugin (name@marketplace) that no agent uses is re-pointed at the
// checkout; anything else holding the id is left alone and reported.
func (s *Store) mergePlugin(catalog []Capability, src CapSource, p sourcePlugin, res *SourceUpdate) []Capability {
	entry := Capability{ID: p.name, Kind: CapPlugin, Path: filepath.Join(s.checkoutDir(src.ID), p.rel), Source: src.ID, Version: p.version, Commit: src.Commit, Description: p.description, Cost: p.cost}
	if _, err := checkCapability(entry); err != nil {
		res.Skipped = append(res.Skipped, p.name+": "+err.Error())
		return catalog
	}
	i := slices.IndexFunc(catalog, func(e Capability) bool { return e.ID == p.name })
	if i < 0 {
		res.Added = append(res.Added, p.name)
		return append(catalog, entry)
	}
	e := catalog[i]
	switch {
	case e.Source == src.ID && fromCheckout(e):
		catalog[i] = refreshEntry(e, entry, res)
	case e.Kind == CapPlugin && e.Plugin == p.name+"@"+src.Marketplace:
		// Only an agent holds it where it is: a group or repo that has it
		// on follows the checkout (agentUsers).
		users, err := s.agentUsers(e.ID)
		if err == nil {
			err = inUse(e.ID, users)
		}
		if err != nil {
			res.Skipped = append(res.Skipped, err.Error())
			return catalog
		}
		entry.SideEffects = e.SideEffects
		catalog[i] = entry
		res.Added = append(res.Added, p.name)
	default:
		res.Skipped = append(res.Skipped, p.name+": the id is taken by another entry")
	}
	return catalog
}

// refreshEntry is old, an entry of this source, after the sync: at the new
// version, or (pinned) unchanged and reported behind.
func refreshEntry(old, fresh Capability, res *SourceUpdate) Capability {
	if old.Pinned {
		if old.Version != fresh.Version {
			res.Behind = append(res.Behind, VersionChange{ID: old.ID, From: old.Version, To: fresh.Version})
		}
		return old
	}
	if old.Version != fresh.Version {
		res.Changed = append(res.Changed, VersionChange{ID: old.ID, From: old.Version, To: fresh.Version})
	}
	fresh.SideEffects = old.SideEffects
	return fresh
}
