package agent

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"slices"
)

// ErrNotPinnable refuses pinning an entry no git source keeps: there is no
// version for it to stay at.
var ErrNotPinnable = errors.New("only an entry from a git source can be pinned")

// PinCapability pins entry id at the commit its folder is at, or unpins it
// back to the source's checkout at the source's current version. A pin
// points the entry into a worktree of the checkout at that commit
// (pinFolder), so a source update never moves it.
func (s *Store) PinCapability(ctx context.Context, id string, pinned bool) (Capability, error) {
	gitMu.Lock()
	defer gitMu.Unlock()
	capMu.Lock()
	defer capMu.Unlock()
	catalog, err := s.Capabilities()
	if err != nil {
		return Capability{}, err
	}
	i := slices.IndexFunc(catalog, func(e Capability) bool { return e.ID == id })
	if i < 0 {
		return Capability{}, ErrNotFound
	}
	e := catalog[i]
	if !fromCheckout(e) {
		return e, fmt.Errorf("%w: %s", ErrNotPinnable, id)
	}
	if e.Pinned == pinned {
		return e, nil
	}
	if pinned {
		e, err = s.pin(ctx, e)
	} else {
		e, err = s.unpin(e)
	}
	if err != nil {
		return Capability{}, err
	}
	catalog[i] = e
	return e, s.writeCapabilities(catalog)
}

func (s *Store) pin(ctx context.Context, e Capability) (Capability, error) {
	checkout := s.checkoutDir(e.Source)
	rel, err := insideFolder(checkout, e.Path)
	if err != nil {
		return e, err
	}
	dir, err := pinFolder(ctx, checkout, s.pinsDir(e.Source), e.Commit)
	if err != nil {
		return e, err
	}
	e.Path, e.Pinned = filepath.Join(dir, rel), true
	return e, nil
}

func (s *Store) unpin(e Capability) (Capability, error) {
	rel, err := insideFolder(filepath.Join(s.pinsDir(e.Source), e.Commit), e.Path)
	if err != nil {
		return e, err
	}
	list, err := s.Sources()
	if err != nil {
		return e, err
	}
	if i := slices.IndexFunc(list, func(c CapSource) bool { return c.ID == e.Source }); i >= 0 && list[i].Commit != "" {
		e.Commit = list[i].Commit
		if v := list[i].Plugins[e.ID]; v != "" {
			e.Version = v
		}
	}
	e.Path, e.Pinned = filepath.Join(s.checkoutDir(e.Source), rel), false
	e.Cost = pluginCost(e.Path, -1)
	return e, nil
}

// insideFolder is path relative to dir, refused when it is not inside it.
func insideFolder(dir, path string) (string, error) {
	rel, err := filepath.Rel(dir, path)
	if err != nil || !filepath.IsLocal(rel) {
		return "", fmt.Errorf("%s is not inside %s", path, dir)
	}
	return rel, nil
}
