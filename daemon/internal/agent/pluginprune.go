package agent

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// PruneInstalledPlugins drops the records Claude Code keeps of plugins
// installed for one folder (scope "local") whose folder is gone and was
// under worktrees: a task's or thread's worktree, removed by its tidy-up.
// Claude Code adds one for every worktree a session starts in and never
// removes it, so the list only grew (222 records for 26 folders, seen
// 2026-11-23). A plugin left with no record goes too. Every other record and
// field is kept as it was. Nothing is written when nothing is gone, and
// nothing when the file changed while it was read: Claude Code writes it
// too, and the next sweep tries again. A file that does not parse is an
// error and is left alone. Returns how many records were removed.
func PruneInstalledPlugins(path, worktrees string) (int, error) {
	if worktrees == "" {
		return 0, nil
	}
	before, err := os.Stat(path)
	if errors.Is(err, os.ErrNotExist) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return 0, err
	}
	out, removed, err := pruneRecords(raw, filepath.Clean(worktrees)+string(filepath.Separator))
	if err != nil || removed == 0 {
		return 0, err
	}
	if now, err := os.Stat(path); err != nil || !now.ModTime().Equal(before.ModTime()) || now.Size() != before.Size() {
		return 0, err // changed under us: leave it to the next sweep
	}
	return removed, replaceFile(path, out, before.Mode().Perm())
}

// pruneRecords is PruneInstalledPlugins on the file's bytes: the new bytes
// and how many records went. under ends in a separator, so a folder that
// only shares its prefix is not under it.
func pruneRecords(raw []byte, under string) ([]byte, int, error) {
	var doc map[string]json.RawMessage
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil, 0, fmt.Errorf("installed plugins: %w", err)
	}
	var plugins map[string][]map[string]json.RawMessage
	if err := json.Unmarshal(doc["plugins"], &plugins); err != nil {
		return nil, 0, fmt.Errorf("installed plugins: %w", err)
	}
	removed := 0
	for id, records := range plugins {
		kept := records[:0]
		for _, r := range records {
			if goneWorktreeRecord(r, under) {
				removed++
				continue
			}
			kept = append(kept, r)
		}
		if len(kept) == 0 {
			delete(plugins, id)
		} else {
			plugins[id] = kept
		}
	}
	if removed == 0 {
		return raw, 0, nil
	}
	return encodePlugins(doc, plugins, removed)
}

// encodePlugins puts plugins back into doc and encodes it the way Claude
// Code writes the file (two-space indent).
func encodePlugins(doc map[string]json.RawMessage, plugins map[string][]map[string]json.RawMessage, removed int) ([]byte, int, error) {
	p, err := json.Marshal(plugins)
	if err != nil {
		return nil, 0, err
	}
	doc["plugins"] = p
	flat, err := json.Marshal(doc)
	if err != nil {
		return nil, 0, err
	}
	var out bytes.Buffer
	if err := json.Indent(&out, flat, "", "  "); err != nil {
		return nil, 0, err
	}
	return out.Bytes(), removed, nil
}

// goneWorktreeRecord: a one-folder record of a folder under under that is
// not there any more. A folder that cannot be looked at for another reason
// counts as there.
func goneWorktreeRecord(r map[string]json.RawMessage, under string) bool {
	var scope, project string
	_ = json.Unmarshal(r["scope"], &scope)
	_ = json.Unmarshal(r["projectPath"], &project)
	dir := filepath.Clean(project) + string(filepath.Separator)
	if scope != "local" || project == "" || dir == under || !strings.HasPrefix(dir, under) {
		return false
	}
	_, err := os.Stat(project)
	return errors.Is(err, os.ErrNotExist)
}

// replaceFile writes data beside path and renames it over path, so a reader
// sees the old file or the new one, never half of one.
func replaceFile(path string, data []byte, mode os.FileMode) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".installed_plugins-*.json")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name()) // gone after the rename; cleans up after a failure
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Chmod(mode); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}
