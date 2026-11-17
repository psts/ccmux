package agent

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
)

// A source is a git marketplace: a repo whose .claude-plugin/marketplace.json
// lists plugins. ccmux keeps its own checkout of each under
// ~/.ccmux/capabilities/<id>/ (beside the agents root), and every plugin in
// that checkout becomes a catalog entry pointing at its folder there
// (capsync.go). The records live beside the catalog, in
// capabilitySourcesFile; no file yet means the three seeds.

// CapSource is one source.
type CapSource struct {
	ID  string `json:"id"`
	URL string `json:"url"`
	// Marketplace is the name its marketplace.json gives itself, which is
	// what installed plugin ids say after the @.
	Marketplace string `json:"marketplace,omitempty"`
	// Commit is what ccmux's checkout is at, Updated when it got there
	// (RFC 3339), and Plugins each plugin's version there, so a pinned
	// entry can tell it is behind.
	Commit  string            `json:"commit,omitempty"`
	Updated string            `json:"updated,omitempty"`
	Plugins map[string]string `json:"plugins,omitempty"`
}

const capabilitySourcesFile = ".ccmux/capability-sources.json"

// seedSources are the sources a host starts with.
var seedSources = []CapSource{
	{ID: "personal", URL: "https://github.com/sanlabsio/claude-personal"},
	{ID: "claude-plugins-official", URL: "https://github.com/anthropics/claude-plugins-official"},
	{ID: "saac-marketplace", URL: "https://github.com/Ryan-SAAS/saac-plugins"},
}

// ErrSourceExists refuses adding a source whose id is taken.
var ErrSourceExists = errors.New("source exists")

// checkoutsDir holds ccmux's checkouts, one folder per source, and the
// pinned worktrees under pinsFolder.
func (s *Store) checkoutsDir() string { return filepath.Join(filepath.Dir(s.Root), "capabilities") }

const pinsFolder = ".pins"

func (s *Store) checkoutDir(id string) string { return filepath.Join(s.checkoutsDir(), id) }

func (s *Store) pinsDir(id string) string { return filepath.Join(s.checkoutsDir(), pinsFolder, id) }

// Sources reads the source records, sorted by id. No file yet is the
// seeds; an empty list stays empty, so removing all three keeps them gone.
func (s *Store) Sources() ([]CapSource, error) {
	var doc struct {
		Sources []CapSource `json:"sources"`
	}
	path := filepath.Join(s.Root, capabilitySourcesFile)
	if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
		doc.Sources = slices.Clone(seedSources)
	} else if err := readJSONIfPresent(path, &doc); err != nil {
		return nil, err
	}
	sort.Slice(doc.Sources, func(i, j int) bool { return doc.Sources[i].ID < doc.Sources[j].ID })
	return orEmptySources(doc.Sources), nil
}

func orEmptySources(list []CapSource) []CapSource {
	if list == nil {
		return []CapSource{}
	}
	return list
}

func (s *Store) writeSources(list []CapSource) error {
	path := filepath.Join(s.Root, capabilitySourcesFile)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	sort.Slice(list, func(i, j int) bool { return list[i].ID < list[j].ID })
	blob, _ := json.MarshalIndent(struct {
		Sources []CapSource `json:"sources"`
	}{orEmptySources(list)}, "", "  ")
	_, err := writeIfChanged(path, append(blob, '\n'))
	return err
}

// AddSource records a new source; its checkout comes with its first
// update (UpdateSource).
func (s *Store) AddSource(src CapSource) (CapSource, error) {
	src = CapSource{ID: src.ID, URL: strings.TrimSpace(src.URL)}
	if err := checkSource(src); err != nil {
		return src, err
	}
	capMu.Lock()
	defer capMu.Unlock()
	list, err := s.Sources()
	if err != nil {
		return src, err
	}
	if slices.ContainsFunc(list, func(e CapSource) bool { return e.ID == src.ID }) {
		return src, fmt.Errorf("%w: %s", ErrSourceExists, src.ID)
	}
	return src, s.writeSources(append(list, src))
}

// checkSource refuses an id the catalog cannot hold and a url that is not
// plain https or an absolute local path. A url with a user or password in
// it is refused too: the records are served to every lens.
func checkSource(src CapSource) error {
	if !capabilityID.MatchString(src.ID) || src.ID == pinsFolder {
		return fmt.Errorf("source id %q: use 1 to 64 chars of letters, digits, _ . and -", src.ID)
	}
	if filepath.IsAbs(src.URL) {
		return nil
	}
	u, err := url.Parse(src.URL)
	if err != nil || u.Scheme != "https" || u.Host == "" {
		return fmt.Errorf("source %s: the url must be https:// or an absolute path", src.ID)
	}
	if u.User != nil {
		return fmt.Errorf("source %s: the url holds a user or password; the catalog holds no secrets, let git's credential helper supply them", src.ID)
	}
	return nil
}

// RemoveSource drops a source, its entries and its checkout. It refuses
// while anything uses one of its entries (CapabilityUsers).
func (s *Store) RemoveSource(id string) error {
	gitMu.Lock()
	defer gitMu.Unlock()
	capMu.Lock()
	defer capMu.Unlock()
	list, err := s.Sources()
	if err != nil {
		return err
	}
	i := slices.IndexFunc(list, func(e CapSource) bool { return e.ID == id })
	if i < 0 {
		return ErrNotFound
	}
	catalog, err := s.Capabilities()
	if err != nil {
		return err
	}
	keep := catalog[:0:0]
	for _, c := range catalog {
		if c.Source != id || c.Commit == "" {
			keep = append(keep, c)
		} else if err := s.refuseInUse(c.ID); err != nil {
			return err
		}
	}
	if err := s.writeCapabilities(keep); err != nil {
		return err
	}
	if err := s.writeSources(slices.Delete(list, i, i+1)); err != nil {
		return err
	}
	return errors.Join(os.RemoveAll(s.checkoutDir(id)), os.RemoveAll(s.pinsDir(id)))
}
